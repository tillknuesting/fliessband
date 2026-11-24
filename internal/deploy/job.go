package deploy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// PostgresImage runs the throwaway database on local clusters.
const PostgresImage = "postgres:17-alpine"

// job is one deployment in progress.
type job struct {
	d   *Deployer
	dep *Deployment
	t   Target
	svc Service

	work      string // rendered overlays
	dockerEnv []string
	database  bool // a factory-run PostgreSQL goes with the service

	prevManifests string // what the live version was applied with, for rollback
}

// errRolledBack ends a deployment whose version never became ready after
// the previous version was restored.
var errRolledBack = errors.New("rolled back")

func (j *job) run(ctx context.Context) {
	j.work = filepath.Join(j.d.dir, j.dep.ID)
	type stage struct {
		id string
		fn func(context.Context, *Step) error
	}
	for _, s := range []stage{{"preflight", j.preflight}, {"image", j.image}, {"load", j.load}, {"database", j.databaseStep},
		{"apply", j.apply}, {"rollout", j.rollout}, {"verify", j.verify}} {
		st := j.step(s.id)
		j.set(st, "running", "")
		start := time.Now()
		err := s.fn(ctx, st)
		j.dep.mu.Lock()
		st.DurationMs = float64(time.Since(start).Milliseconds())
		j.dep.mu.Unlock()
		switch {
		case errors.Is(err, errSkip):
			j.set(st, "skipped", st.Summary)
		case errors.Is(err, errRolledBack):
			j.set(st, "failed", st.Summary)
			j.skipRest()
			j.d.finish(j.dep, RolledBack, st.Summary)
			return
		case err != nil:
			j.logf("%s failed: %v", st.Name, err)
			j.set(st, "failed", err.Error())
			j.skipRest()
			j.d.finish(j.dep, Failed, st.Name+": "+err.Error())
			return
		default:
			j.set(st, "passed", st.Summary)
		}
		_ = j.d.save(j.dep)
	}
	j.d.finish(j.dep, Live, "")
}

var errSkip = errors.New("skip")

func (j *job) step(id string) *Step {
	for _, s := range j.dep.Steps {
		if s.ID == id {
			return s
		}
	}
	panic("no step " + id)
}

func (j *job) set(st *Step, status, summary string) {
	j.dep.mu.Lock()
	st.Status = status
	if summary != "" {
		st.Summary = summary
	}
	j.dep.mu.Unlock()
}

func (j *job) summary(st *Step, format string, a ...any) {
	j.dep.mu.Lock()
	st.Summary = fmt.Sprintf(format, a...)
	j.dep.mu.Unlock()
}

func (j *job) skipRest() {
	j.dep.mu.Lock()
	defer j.dep.mu.Unlock()
	for _, s := range j.dep.Steps {
		if s.Status == "pending" {
			s.Status = "skipped"
		}
	}
}

func (j *job) logf(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	j.dep.mu.Lock()
	j.dep.Log = append(j.dep.Log, line)
	if n := len(j.dep.Log); n > 600 {
		j.dep.Log = j.dep.Log[n-600:]
	}
	j.dep.mu.Unlock()
}

// exec runs a command, logging it and the end of its output.
func (j *job) exec(ctx context.Context, env []string, timeout time.Duration, stdin []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	j.logf("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.WaitDelay = env, 5*time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := out.String()
	for _, l := range tail(text, 12) {
		j.logf("  %s", l)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("%s did not finish within %s", name, timeout)
	}
	if err != nil {
		if l := lastLine(text); l != "" {
			return text, fmt.Errorf("%s: %s", name, l)
		}
		return text, fmt.Errorf("%s: %w", name, err)
	}
	return text, nil
}

// quietExec runs a command without logging it or its output.
func (j *job) quietExec(ctx context.Context, env []string, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.WaitDelay = env, 5*time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %s", name, lastLine(string(out)))
	}
	return string(out), nil
}

func (j *job) kubectl(ctx context.Context, _ *Step, timeout time.Duration, args ...string) (string, error) {
	return j.exec(ctx, j.d.baseEnv, timeout, nil, "kubectl", append([]string{"--context", j.t.Context, "-n", j.t.Namespace}, args...)...)
}

func (j *job) imageRef() string {
	if j.t.Strategy == Registry {
		return strings.TrimSuffix(j.t.Registry, "/") + "/" + j.svc.Name + ":" + j.svc.Run
	}
	return "fliessband/" + j.svc.Name + ":" + j.svc.Run
}

// preflight checks the run's files, the tools and the cluster.
func (j *job) preflight(ctx context.Context, st *Step) error {
	for _, f := range []string{"Dockerfile", "deploy/k8s/kustomization.yaml"} {
		if _, err := os.Stat(filepath.Join(j.svc.Dir, f)); err != nil {
			return fmt.Errorf("the run has no %s: turn on the docker and kubernetes features in the spec", f)
		}
	}
	need := []string{"kubectl", "docker"}
	switch j.t.Strategy {
	case K3d, Kind, Minikube:
		need = append(need, string(j.t.Strategy))
	}
	for _, n := range need {
		if _, err := exec.LookPath(n); err != nil {
			return fmt.Errorf("%s is not installed", n)
		}
	}
	env, err := j.d.dockerEnv()
	if err != nil {
		return err
	}
	j.dockerEnv = env
	j.logf("$ kubectl --context %s version", j.t.Context)
	out, err := j.quietExec(ctx, j.d.baseEnv, 15*time.Second, "kubectl", "--context", j.t.Context, "--request-timeout=10s",
		"version", "-o", "yaml")
	if err != nil {
		return fmt.Errorf("cluster %s is not reachable: %w", j.t.Context, err)
	}
	server := "?"
	if m := regexp.MustCompile(`(?s)serverVersion:.*?gitVersion: (\S+)`).FindStringSubmatch(out); m != nil {
		server = m[1]
	}
	if j.svc.Postgres {
		// A secret the user made is theirs: the service uses it and the
		// factory runs no database. Otherwise local clusters get a
		// throwaway PostgreSQL; remote ones must have the secret.
		owner, _ := j.kubectl(ctx, st, 15*time.Second, "get", "secret", j.svc.Name+"-db", "--ignore-not-found",
			"-o", `jsonpath={.metadata.labels.app\.kubernetes\.io/managed-by}{"|"}{.metadata.name}`)
		switch {
		case strings.HasSuffix(strings.TrimSpace(owner), "|"+j.svc.Name+"-db") && !strings.HasPrefix(owner, "fliessband|"):
			j.logf("secret %s-db exists and is not the factory's: the service uses that database", j.svc.Name)
		case j.t.Local:
			j.database = true
		default:
			return fmt.Errorf("the service needs PostgreSQL: create the secret %s-db with key url in namespace %s (kubectl -n %s create secret generic %s-db --from-literal=url=postgres://...)",
				j.svc.Name, j.t.Namespace, j.t.Namespace, j.svc.Name)
		}
	}
	j.summary(st, "%s (Kubernetes %s), namespace %s", j.t.Context, server, j.t.Namespace)
	return nil
}

// image builds the service's image, unless this run's image exists.
func (j *job) image(ctx context.Context, st *Step) error {
	img := j.imageRef()
	j.dep.mu.Lock()
	j.dep.Image = img
	j.dep.mu.Unlock()
	if _, err := j.exec(ctx, j.dockerEnv, 20*time.Second, nil, "docker", "image", "inspect", "--format", "{{.Id}}", img); err == nil {
		j.summary(st, "%s, built before", img)
		return nil
	}
	if _, err := j.exec(ctx, j.dockerEnv, 15*time.Minute, nil, "docker", "build", "--quiet",
		"--label", "org.opencontainers.image.revision="+j.svc.Run, "--label", "dev.fliessband.run="+j.svc.Run,
		"--label", "dev.fliessband.service="+j.svc.Name, "-t", img, j.svc.Dir); err != nil {
		return err
	}
	size, _ := j.exec(ctx, j.dockerEnv, 20*time.Second, nil, "docker", "image", "inspect", "--format", "{{.Size}}", img)
	n, _ := strconv.ParseFloat(strings.TrimSpace(size), 64)
	j.summary(st, "%s, %.1f MB", img, n/1e6)
	return nil
}

// load gets the image onto the cluster's nodes.
func (j *job) load(ctx context.Context, st *Step) error {
	img := j.imageRef()
	var err error
	switch j.t.Strategy {
	case K3d:
		// k3d reports success even when a node missed the image (one that
		// was restarting, say), so check every node and import once more.
		for attempt := range 2 {
			if _, err = j.exec(ctx, j.dockerEnv, 5*time.Minute, nil, "k3d", "image", "import", "--cluster", j.t.Cluster, img); err != nil {
				break
			}
			var missing []string
			if missing, err = j.k3dMissing(ctx, img); err != nil || len(missing) == 0 {
				break
			}
			err = fmt.Errorf("the image did not reach node %s", strings.Join(missing, ", "))
			if attempt == 0 {
				j.logf("%v; importing again", err)
				time.Sleep(3 * time.Second)
			}
		}
	case Kind:
		_, err = j.exec(ctx, j.dockerEnv, 5*time.Minute, nil, "kind", "load", "docker-image", "--name", j.t.Cluster, img)
	case Minikube:
		_, err = j.exec(ctx, append(j.dockerEnv, j.d.baseEnv...), 5*time.Minute, nil, "minikube", "image", "load", "-p", j.t.Cluster, img)
	case Shared:
		j.summary(st, "the cluster shares the build engine's images")
		return nil
	case Registry:
		// Pushing needs the user's registry login, which the factory's
		// isolated Docker client does not carry.
		env := []string{}
		for _, e := range j.dockerEnv {
			if !strings.HasPrefix(e, "DOCKER_CONFIG=") {
				env = append(env, e)
			}
		}
		_, err = j.exec(ctx, env, 10*time.Minute, nil, "docker", "push", "--quiet", img)
	}
	if err != nil {
		return err
	}
	j.summary(st, "%s into %s", img, j.t.Context)
	return nil
}

// k3dMissing lists the cluster's nodes that do not have the image.
func (j *job) k3dMissing(ctx context.Context, img string) ([]string, error) {
	out, err := j.quietExec(ctx, j.dockerEnv, 20*time.Second, "docker", "ps", "--filter", "label=k3d.cluster="+j.t.Cluster,
		"--format", `{{.Names}} {{.Label "k3d.role"}}`)
	if err != nil {
		return nil, err
	}
	ref := img
	if strings.Count(strings.SplitN(img, ":", 2)[0], "/") == 1 {
		ref = "docker.io/" + img
	}
	var missing []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || (f[1] != "server" && f[1] != "agent") {
			continue
		}
		if _, err := j.quietExec(ctx, j.dockerEnv, 20*time.Second, "docker", "exec", f[0], "crictl", "inspecti", ref); err != nil {
			missing = append(missing, f[0])
		}
	}
	return missing, nil
}

// kustomization renders an overlay. Its labels go on everything the
// factory applies; the run label also goes on pod templates.
func (j *job) kustomization(resources []string, component string, image bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: %s\nresources:\n", j.t.Namespace)
	for _, r := range resources {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	fmt.Fprintf(&b, "labels:\n  - pairs:\n      app.kubernetes.io/managed-by: fliessband\n      fliessband.dev/service: %s\n", j.svc.Name)
	if component != "" {
		fmt.Fprintf(&b, "      fliessband.dev/component: %s\n", component)
	}
	b.WriteString("    includeSelectors: false\n")
	if component == "" { // the database's pods must not restart with each version: they hold its data
		fmt.Fprintf(&b, "  - pairs:\n      fliessband.dev/run: %q\n    includeSelectors: false\n    includeTemplates: true\n", j.svc.Run)
	}
	if image {
		repo, tag := strings.TrimSuffix(j.imageRef(), ":"+j.svc.Run), j.svc.Run
		fmt.Fprintf(&b, "images:\n  - name: %s\n    newName: %s\n    newTag: %q\n", j.svc.Name, repo, tag)
	}
	return b.String()
}

// ensureNamespace creates the target namespace with the restricted Pod
// Security Standard enforced, unless it exists.
func (j *job) ensureNamespace(ctx context.Context, st *Step) error {
	out, err := j.exec(ctx, j.d.baseEnv, 20*time.Second, nil, "kubectl", "--context", j.t.Context, "get", "namespace", j.t.Namespace,
		"--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return nil
	}
	ns := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    app.kubernetes.io/managed-by: fliessband
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/warn: restricted
`, j.t.Namespace)
	_, err = j.exec(ctx, j.d.baseEnv, 30*time.Second, []byte(ns), "kubectl", "--context", j.t.Context, "apply", "-f", "-")
	if err == nil {
		j.logf("created namespace %s with the restricted pod security standard", j.t.Namespace)
	}
	return err
}

// databaseStep runs a throwaway PostgreSQL next to the service on local
// clusters: data lives in an emptyDir and is lost when its pod goes.
func (j *job) databaseStep(ctx context.Context, st *Step) error {
	if !j.database {
		if j.svc.Postgres {
			j.summary(st, "uses the existing %s-db secret", j.svc.Name)
		} else {
			j.summary(st, "the service keeps its data in memory")
		}
		return errSkip
	}
	if err := j.ensureNamespace(ctx, st); err != nil {
		return err
	}
	dir := filepath.Join(j.work, "db")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	res := []string{"database.yaml"}
	exists, err := j.kubectl(ctx, st, 15*time.Second, "get", "secret", j.svc.Name+"-db", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(exists) == "" {
		pw := make([]byte, 18)
		_, _ = rand.Read(pw)
		p := hex.EncodeToString(pw)
		secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %[1]s-db\ntype: Opaque\nstringData:\n  password: %[2]s\n  url: postgres://postgres:%[2]s@%[1]s-db:5432/postgres?sslmode=disable\n", j.svc.Name, p)
		if err := os.WriteFile(filepath.Join(dir, "secret.yaml"), []byte(secret), 0o600); err != nil {
			return err
		}
		res = append(res, "secret.yaml")
	}
	if err := os.WriteFile(filepath.Join(dir, "database.yaml"), []byte(databaseManifest(j.svc.Name)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(j.kustomization(res, "database", false)), 0o600); err != nil {
		return err
	}
	if _, err := j.kubectl(ctx, st, time.Minute, "apply", "-k", dir); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, "secret.yaml")) // the password lives in the cluster only
	if _, err := j.kubectl(ctx, st, 3*time.Minute, "rollout", "status", "deployment/"+j.svc.Name+"-db", "--timeout=170s"); err != nil {
		return fmt.Errorf("PostgreSQL did not become ready: %w", err)
	}
	j.summary(st, "PostgreSQL %s, throwaway (emptyDir)", strings.TrimPrefix(PostgresImage, "postgres:"))
	return nil
}

func databaseManifest(name string) string {
	return fmt.Sprintf(`# A throwaway PostgreSQL for a local cluster: its data is lost with its pod.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s-db
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: %[1]s-db
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %[1]s-db
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 70
        runAsGroup: 70
        fsGroup: 70
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: postgres
          image: %[2]s
          env:
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: %[1]s-db
                  key: password
            - name: PGDATA
              value: /var/lib/postgresql/data/pgdata
          ports:
            - name: postgres
              containerPort: 5432
          readinessProbe:
            exec:
              command: [pg_isready, -U, postgres, -h, 127.0.0.1]
            periodSeconds: 2
          resources:
            requests:
              cpu: 50m
              memory: 128Mi
            limits:
              memory: 512Mi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
            - name: run
              mountPath: /var/run/postgresql
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: data
          emptyDir: {}
        - name: run
          emptyDir: {}
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s-db
spec:
  selector:
    app.kubernetes.io/name: %[1]s-db
  ports:
    - name: postgres
      port: 5432
      targetPort: postgres
`, name, PostgresImage)
}

// apply renders the overlay over the run's manifests and applies it.
func (j *job) apply(ctx context.Context, st *Step) error {
	if err := j.ensureNamespace(ctx, st); err != nil {
		return err
	}
	base := filepath.Join(j.work, "base")
	if err := copyDir(filepath.Join(j.svc.Dir, "deploy", "k8s"), base); err != nil {
		return err
	}
	app := filepath.Join(j.work, "app")
	if err := os.MkdirAll(app, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(app, "kustomization.yaml"), []byte(j.kustomization([]string{"../base"}, "", true)), 0o600); err != nil {
		return err
	}
	j.logf("$ kubectl kustomize %s", app)
	rendered, err := j.quietExec(ctx, j.d.baseEnv, 30*time.Second, "kubectl", "kustomize", app)
	if err != nil {
		return err
	}
	j.dep.mu.Lock()
	j.dep.Manifests = rendered
	j.dep.mu.Unlock()
	out, err := j.kubectl(ctx, st, time.Minute, "apply", "-k", app)
	if err != nil {
		return err
	}
	changed := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(l, " configured") || strings.HasSuffix(l, " created") {
			changed++
		}
	}
	j.summary(st, "%d of %d objects changed", changed, strings.Count(rendered, "\nkind: ")+boolInt(strings.HasPrefix(rendered, "kind: ")))
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rollout waits for the new version. When it does not become ready, the
// pods are diagnosed and the previous version's manifests applied again.
func (j *job) rollout(ctx context.Context, st *Step) error {
	why, err := j.waitRollout(ctx, j.svc.Run, true)
	if err == nil {
		rep, _ := j.quietExec(ctx, j.d.baseEnv, 15*time.Second, "kubectl", "--context", j.t.Context, "-n", j.t.Namespace,
			"get", "deployment", j.svc.Name, "-o", "jsonpath={.status.readyReplicas}/{.spec.replicas}")
		j.dep.mu.Lock()
		j.dep.Replicas = strings.TrimSpace(rep)
		j.dep.mu.Unlock()
		j.summary(st, "%s replicas ready", strings.TrimSpace(rep))
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	if r := j.diagnose(ctx, st); why == "" {
		why = r
	}
	if j.dep.Previous == "" {
		return fmt.Errorf("the new version did not become ready: %s", why)
	}
	j.logf("restoring the previous version (run %s)", j.dep.Previous)
	if j.prevManifests != "" {
		_, err = j.exec(ctx, j.d.baseEnv, time.Minute, []byte(j.prevManifests), "kubectl", "--context", j.t.Context, "-n", j.t.Namespace, "apply", "-f", "-")
	} else {
		_, err = j.kubectl(ctx, st, 30*time.Second, "rollout", "undo", "deployment/"+j.svc.Name)
	}
	if err != nil {
		return fmt.Errorf("the new version did not become ready (%s), and rolling back failed: %w", why, err)
	}
	if _, err := j.waitRollout(ctx, j.dep.Previous, false); err != nil {
		return fmt.Errorf("the new version did not become ready (%s), and the previous one did not come back: %w", why, err)
	}
	j.summary(st, "not ready (%s); run %s serves again", why, j.dep.Previous)
	return errRolledBack
}

// fatal are pod states that do not heal by waiting.
var fatal = map[string]bool{"CreateContainerConfigError": true, "CreateContainerError": true, "InvalidImageName": true, "ErrImageNeverPull": true}

// waitRollout polls the deployment until all its replicas run the given
// version and are available. With failFast it stops as soon as a pod of
// that version is clearly broken, and returns the reason.
func (j *job) waitRollout(ctx context.Context, run string, failFast bool) (string, error) {
	deadline := time.Now().Add(j.d.RolloutTimeout)
	j.logf("waiting up to %s for run %s to be ready", j.d.RolloutTimeout, run)
	last, pulls := "", 0
	for {
		out, err := j.quietExec(ctx, j.d.baseEnv, 15*time.Second, "kubectl", "--context", j.t.Context, "-n", j.t.Namespace,
			"get", "deployment", j.svc.Name, "-o", "json")
		if err == nil {
			p := rolloutProgress([]byte(out), run)
			if p.line != last {
				j.logf("  %s", p.line)
				last = p.line
			}
			if p.done {
				return "", nil
			}
		}
		if failFast {
			reason, restarts, err := j.podTrouble(ctx, run)
			switch {
			case err != nil:
			case fatal[reason], reason == "CrashLoopBackOff" && restarts >= 2:
				return reason, fmt.Errorf("pods of the new version are in %s", reason)
			case reason == "ImagePullBackOff" || reason == "ErrImagePull":
				if pulls++; pulls >= 5 {
					return reason, fmt.Errorf("pods of the new version are in %s", reason)
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("run %s was not ready within %s", run, j.d.RolloutTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

type progress struct {
	done bool
	line string
}

// rolloutProgress reads `kubectl get deployment -o json`.
func rolloutProgress(b []byte, run string) progress {
	var d struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Template struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			Replicas           int   `json:"replicas"`
			Updated            int   `json:"updatedReplicas"`
			Ready              int   `json:"readyReplicas"`
			Available          int   `json:"availableReplicas"`
		} `json:"status"`
	}
	if json.Unmarshal(b, &d) != nil {
		return progress{line: "reading the deployment failed"}
	}
	want := 1
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	p := progress{line: fmt.Sprintf("%d of %d updated, %d available, %d old", s.Updated, want, s.Available, max(0, s.Replicas-s.Updated))}
	p.done = d.Spec.Template.Metadata.Labels["fliessband.dev/run"] == run && s.ObservedGeneration >= d.Metadata.Generation &&
		s.Updated == want && s.Available == want && s.Replicas == want
	return p
}

// podTrouble returns the worst waiting reason among the version's pods.
func (j *job) podTrouble(ctx context.Context, run string) (string, int, error) {
	out, err := j.quietExec(ctx, j.d.baseEnv, 15*time.Second, "kubectl", "--context", j.t.Context, "-n", j.t.Namespace,
		"get", "pods", "-l", "app.kubernetes.io/name="+j.svc.Name+",fliessband.dev/run="+run, "-o", "json")
	if err != nil {
		return "", 0, err
	}
	return worstPod([]byte(out))
}

func worstPod(b []byte) (string, int, error) {
	var pods struct {
		Items []struct {
			Status struct {
				ContainerStatuses []struct {
					RestartCount int `json:"restartCount"`
					State        struct {
						Waiting *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &pods); err != nil {
		return "", 0, err
	}
	reason, restarts := "", 0
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if c.State.Waiting == nil || c.State.Waiting.Reason == "" || c.State.Waiting.Reason == "ContainerCreating" {
				continue
			}
			if fatal[c.State.Waiting.Reason] || c.RestartCount >= restarts {
				reason, restarts = c.State.Waiting.Reason, c.RestartCount
			}
		}
	}
	return reason, restarts, nil
}

// diagnose finds why pods are not ready, from their state, events and logs.
func (j *job) diagnose(ctx context.Context, st *Step) string {
	sel := "app.kubernetes.io/name=" + j.svc.Name + ",fliessband.dev/run=" + j.svc.Run
	reason := "rollout timed out"
	if out, err := j.kubectl(ctx, st, 15*time.Second, "get", "pods", "-l", sel, "-o",
		`jsonpath={range .items[*]}{range .status.containerStatuses[*]}{.state.waiting.reason}{" "}{.lastState.terminated.reason}{"\n"}{end}{end}`); err == nil {
		for _, l := range strings.Fields(out) {
			if l != "" {
				reason = l
				break
			}
		}
	}
	names, _ := j.quietExec(ctx, j.d.baseEnv, 15*time.Second, "kubectl", "--context", j.t.Context, "-n", j.t.Namespace, "get", "pods",
		"-l", sel, "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
	ours := map[string]bool{}
	for n := range strings.FieldsSeq(names) {
		ours[n] = true
	}
	if out, err := j.quietExec(ctx, j.d.baseEnv, 15*time.Second, "kubectl", "--context", j.t.Context, "-n", j.t.Namespace, "get", "events",
		"--field-selector", "type=Warning,involvedObject.kind=Pod", "--sort-by=.lastTimestamp", "-o",
		`custom-columns=POD:.involvedObject.name,REASON:.reason,MESSAGE:.message`, "--no-headers"); err == nil {
		var lines []string
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 1 && ours[f[0]] {
				lines = append(lines, l)
			}
		}
		for _, l := range lines[max(0, len(lines)-8):] {
			j.logf("  event %s", l)
		}
	}
	_, _ = j.kubectl(ctx, st, 20*time.Second, "logs", "-l", sel, "--tail=20", "--prefix", "--all-containers")
	return reason
}

// verify calls the live service's health check through a port-forward.
func (j *job) verify(ctx context.Context, st *Step) error {
	if !j.svc.Health {
		j.summary(st, "the spec has no health service to call")
		return errSkip
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	args := []string{"--context", j.t.Context, "-n", j.t.Namespace, "port-forward", "--address", "127.0.0.1",
		"service/" + j.svc.Name, "0:" + strconv.Itoa(j.svc.Port)}
	j.logf("$ kubectl %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env, cmd.WaitDelay = j.d.baseEnv, 3*time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		re := regexp.MustCompile(`Forwarding from (127\.0\.0\.1:\d+)`)
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case addr <- m[1]:
				default:
				}
			}
		}
	}()
	var a string
	select {
	case a = <-addr:
	case <-ctx.Done():
		return errors.New("kubectl port-forward did not start")
	}
	conn, err := grpc.NewClient(a, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	hc := healthpb.NewHealthClient(conn)
	var last error
	for range 5 {
		cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		start := time.Now()
		resp, err := hc.Check(cctx, &healthpb.HealthCheckRequest{Service: j.svc.FullName})
		ccancel()
		if err == nil {
			status := resp.GetStatus().String()
			j.dep.mu.Lock()
			j.dep.Health = status
			j.dep.mu.Unlock()
			j.logf("health %s: %s in %s", j.svc.FullName, status, time.Since(start).Round(time.Millisecond))
			if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
				return fmt.Errorf("health status is %s, want SERVING", status)
			}
			j.summary(st, "%s SERVING through the cluster service", j.svc.FullName)
			return nil
		}
		last = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("health check: %w", last)
}

func copyDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := os.ReadFile(p) //nolint:gosec // the run's own manifests
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
}
