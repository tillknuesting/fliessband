package deploy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Service is what the deployer needs to know about a run's service.
type Service struct {
	Run      string // run id, also the image tag
	Name     string // Kubernetes name, e.g. inventory
	FullName string // gRPC service, e.g. acme.inventory.v1.InventoryService
	Port     int
	Health   bool
	Postgres bool
	Dir      string // the generated service: Dockerfile and deploy/k8s
}

// Status of a deployment.
type Status string

const (
	Running    Status = "running"
	Live       Status = "live"        // the version serving on its target
	Failed     Status = "failed"      // never became ready; nothing was live before
	RolledBack Status = "rolled_back" // never became ready; the previous version serves again
	Replaced   Status = "replaced"    // was live until a newer version went live
	Removed    Status = "removed"     // was live until the service was removed
)

// Step is one stage of a deployment.
type Step struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"` // pending, running, passed, failed, skipped
	Summary    string  `json:"summary,omitempty"`
	DurationMs float64 `json:"duration_ms"`
}

// Deployment is one attempt to put a run live on a target, or to remove a
// service from it (Action "remove").
type Deployment struct {
	ID         string     `json:"id"`
	Action     string     `json:"action"` // deploy or remove
	Target     string     `json:"target"`
	Context    string     `json:"context"`
	Namespace  string     `json:"namespace"`
	Service    string     `json:"service"`
	Run        string     `json:"run"`
	Image      string     `json:"image,omitempty"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Previous   string     `json:"previous,omitempty"` // run that was live before
	Error      string     `json:"error,omitempty"`
	Health     string     `json:"health,omitempty"`
	Replicas   string     `json:"replicas,omitempty"` // ready/desired after the rollout
	Steps      []*Step    `json:"steps"`
	Log        []string   `json:"log"`
	Manifests  string     `json:"manifests,omitempty"` // what was applied, rendered by kustomize
	PID        int        `json:"pid,omitempty"`       // the process running it

	mu sync.Mutex
}

// Summary is a deployment without its log and manifests.
type Summary struct {
	ID         string     `json:"id"`
	Action     string     `json:"action"`
	Target     string     `json:"target"`
	Service    string     `json:"service"`
	Run        string     `json:"run"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Previous   string     `json:"previous,omitempty"`
	Error      string     `json:"error,omitempty"`
	Step       string     `json:"step,omitempty"` // the running step
}

// Errors returned by Deploy and Remove.
var (
	ErrNoTarget = errors.New("no such deploy target")
	ErrBusy     = errors.New("a deployment of this service to this target is already running")
)

var steps = []Step{
	{ID: "preflight", Name: "Preflight"},
	{ID: "image", Name: "Image"},
	{ID: "load", Name: "Load"},
	{ID: "database", Name: "Database"},
	{ID: "apply", Name: "Apply"},
	{ID: "rollout", Name: "Rollout"},
	{ID: "verify", Name: "Verify"},
}

// Deployer runs deployments and remembers them in dataDir/deploys.
type Deployer struct {
	dir       string
	targets   []Target
	dockerEnv func() ([]string, error) // the factory's Docker client, for builds
	baseEnv   []string                 // PATH, HOME and KUBECONFIG for kubectl and cluster CLIs

	// RolloutTimeout bounds how long a new version may take to become ready.
	RolloutTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	all  map[string]*Deployment
	seen map[string]time.Time // record file times, for deployments this process did not start
	own  map[string]bool      // deployments this process started
	busy map[string]string    // target/service -> running deployment

	stateMu sync.Mutex
	state   map[string]*TargetState
}

// New loads past deployments. dockerEnv returns the environment of the
// factory's Docker client.
func New(dataDir string, targets []Target, dockerEnv func() ([]string, error)) (*Deployer, error) {
	dir := filepath.Join(dataDir, "deploys")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Deployer{dir: dir, targets: targets, dockerEnv: dockerEnv, baseEnv: BaseEnv(), RolloutTimeout: 150 * time.Second,
		ctx: ctx, cancel: cancel, all: map[string]*Deployment{}, seen: map[string]time.Time{}, own: map[string]bool{},
		busy: map[string]string{}, state: map[string]*TargetState{}}
	d.mu.Lock()
	d.rescanLocked()
	d.mu.Unlock()
	return d, nil
}

// rescanLocked reads deployment records that another process (fliessband
// deploy in a terminal, say) wrote or changed. A record still running whose
// process is gone was interrupted, and is marked so.
func (d *Deployer) rescanLocked() {
	entries, _ := os.ReadDir(d.dir)
	for _, e := range entries {
		id := e.Name()
		if dep, ok := d.all[id]; ok && d.own[id] && dep.status() == Running {
			continue // this process is writing it
		}
		path := filepath.Join(d.dir, id, "deployment.json")
		fi, err := os.Stat(path)
		if err != nil || (!d.seen[id].IsZero() && !fi.ModTime().After(d.seen[id])) {
			continue
		}
		b, err := os.ReadFile(path) //nolint:gosec // the deployer's own records
		if err != nil {
			continue
		}
		dep := &Deployment{}
		if json.Unmarshal(b, dep) != nil || dep.ID != id {
			continue
		}
		d.seen[id] = fi.ModTime()
		if dep.Status == Running && !d.own[id] && !alive(dep.PID) {
			dep.Status, dep.Error = Failed, "interrupted: the process running it stopped"
			now := time.Now().UTC()
			dep.FinishedAt = &now
			_ = d.save(dep)
		}
		d.all[id] = dep
	}
}

// BaseEnv is the environment for kubectl and the cluster CLIs: enough to
// find them and the kubeconfig, and no factory secrets.
func BaseEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, k := range []string{"KUBECONFIG", "XDG_CONFIG_HOME", "TMPDIR", "USER", "MINIKUBE_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Targets returns the configured targets.
func (d *Deployer) Targets() []Target { return slices.Clone(d.targets) }

func (d *Deployer) target(name string) (Target, bool) {
	for _, t := range d.targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// Close cancels running deployments and waits for them.
func (d *Deployer) Close() {
	d.cancel()
	d.wg.Wait()
}

func newID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// Deploy starts deploying svc to the named target and returns at once.
func (d *Deployer) Deploy(targetName string, svc Service) (*Deployment, error) {
	t, ok := d.target(targetName)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrNoTarget, targetName)
	}
	key := t.Name + "/" + svc.Name
	d.mu.Lock()
	d.rescanLocked()
	if d.runningLocked(t.Name, svc.Name) {
		d.mu.Unlock()
		return nil, ErrBusy
	}
	dep := &Deployment{ID: newID(), Action: "deploy", Target: t.Name, Context: t.Context, Namespace: t.Namespace, Service: svc.Name,
		Run: svc.Run, Status: Running, CreatedAt: time.Now().UTC(), Log: []string{}, PID: os.Getpid()}
	prev := ""
	if live := d.liveLocked(t.Name, svc.Name); live != nil {
		dep.Previous = live.Run
		live.mu.Lock()
		prev = live.Manifests
		live.mu.Unlock()
	}
	for _, s := range steps {
		s.Status = "pending"
		if s.ID == "load" && t.Strategy == Registry {
			s.Name = "Push"
		}
		dep.Steps = append(dep.Steps, &s)
	}
	d.all[dep.ID], d.busy[key], d.own[dep.ID] = dep, dep.ID, true
	d.mu.Unlock()
	_ = d.save(dep)

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		job := &job{d: d, dep: dep, t: t, svc: svc, prevManifests: prev}
		job.run(d.ctx)
		d.mu.Lock()
		delete(d.busy, key)
		d.mu.Unlock()
		d.forget(t.Name)
	}()
	return dep, nil
}

// runningLocked reports whether this or another process is deploying
// service to target.
func (d *Deployer) runningLocked(target, service string) bool {
	if _, ok := d.busy[target+"/"+service]; ok {
		return true
	}
	for _, dep := range d.all {
		if dep.Target == target && dep.Service == service && dep.status() == Running {
			return true
		}
	}
	return false
}

// liveLocked returns the deployment serving service on target, or nil.
func (d *Deployer) liveLocked(target, service string) *Deployment {
	for _, dep := range d.all {
		if dep.Target == target && dep.Service == service && dep.Action == "deploy" && dep.status() == Live {
			return dep
		}
	}
	return nil
}

// Live returns the deployment serving service on target, or nil.
func (d *Deployer) Live(target, service string) *Deployment {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rescanLocked()
	return d.liveLocked(target, service)
}

// Get returns a deployment by id, or nil.
func (d *Deployer) Get(id string) *Deployment {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rescanLocked()
	return d.all[id]
}

// List returns summaries, newest first.
func (d *Deployer) List() []Summary {
	d.mu.Lock()
	d.rescanLocked()
	deps := make([]*Deployment, 0, len(d.all))
	for _, dep := range d.all {
		deps = append(deps, dep)
	}
	d.mu.Unlock()
	out := make([]Summary, 0, len(deps))
	for _, dep := range deps {
		out = append(out, dep.Summary())
	}
	slices.SortFunc(out, func(a, b Summary) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

func (dep *Deployment) status() Status {
	dep.mu.Lock()
	defer dep.mu.Unlock()
	return dep.Status
}

// Summary returns the deployment without its log.
func (dep *Deployment) Summary() Summary {
	dep.mu.Lock()
	defer dep.mu.Unlock()
	s := Summary{ID: dep.ID, Action: dep.Action, Target: dep.Target, Service: dep.Service, Run: dep.Run, Status: dep.Status,
		CreatedAt: dep.CreatedAt, FinishedAt: dep.FinishedAt, Previous: dep.Previous, Error: dep.Error}
	for _, st := range dep.Steps {
		if st.Status == "running" {
			s.Step = st.Name
		}
	}
	return s
}

// JSON returns the deployment as JSON.
func (dep *Deployment) JSON() []byte {
	dep.mu.Lock()
	defer dep.mu.Unlock()
	b, _ := json.Marshal(dep)
	return b
}

func (d *Deployer) save(dep *Deployment) error {
	dir := filepath.Join(d.dir, dep.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	b := dep.JSON()
	tmp := filepath.Join(dir, "deployment.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "deployment.json"))
}

// setStatus records a final status and, when a version goes live, marks
// the one it replaces.
func (d *Deployer) finish(dep *Deployment, st Status, errMsg string) {
	d.mu.Lock()
	if st == Live || st == Removed {
		for _, o := range d.all {
			if o != dep && o.Target == dep.Target && o.Service == dep.Service && o.Action == "deploy" && o.status() == Live {
				o.mu.Lock()
				o.Status = map[Status]Status{Live: Replaced, Removed: Removed}[st]
				o.mu.Unlock()
				_ = d.save(o)
			}
		}
	}
	d.mu.Unlock()
	now := time.Now().UTC()
	dep.mu.Lock()
	dep.Status, dep.Error, dep.FinishedAt = st, errMsg, &now
	if dep.Action == "remove" {
		dep.Status = Removed
		if errMsg != "" {
			dep.Status = Failed
		}
	}
	dep.mu.Unlock()
	_ = d.save(dep)
}

// Remove deletes a service the factory deployed from a target.
func (d *Deployer) Remove(ctx context.Context, targetName, service string) (*Deployment, error) {
	t, ok := d.target(targetName)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrNoTarget, targetName)
	}
	if !dnsLabel.MatchString(service) {
		return nil, fmt.Errorf("%q is not a service name", service)
	}
	key := t.Name + "/" + service
	d.mu.Lock()
	d.rescanLocked()
	if d.runningLocked(t.Name, service) {
		d.mu.Unlock()
		return nil, ErrBusy
	}
	dep := &Deployment{ID: newID(), Action: "remove", Target: t.Name, Context: t.Context, Namespace: t.Namespace, Service: service,
		Status: Running, CreatedAt: time.Now().UTC(), Log: []string{}, PID: os.Getpid(), Steps: []*Step{{ID: "remove", Name: "Remove", Status: "running"}}}
	if live := d.liveLocked(t.Name, service); live != nil {
		dep.Run, dep.Previous = live.Run, live.Run
	}
	d.all[dep.ID], d.busy[key], d.own[dep.ID] = dep, dep.ID, true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.busy, key)
		d.mu.Unlock()
		d.forget(t.Name)
	}()
	j := &job{d: d, dep: dep, t: t, svc: Service{Name: service}}
	start := time.Now()
	_, err := j.kubectl(ctx, dep.Steps[0], time.Minute, "delete", "deployment,service,poddisruptionbudget,secret",
		"-l", "app.kubernetes.io/managed-by=fliessband,fliessband.dev/service="+service, "--ignore-not-found", "--wait=false")
	dep.mu.Lock()
	dep.Steps[0].DurationMs = float64(time.Since(start).Milliseconds())
	dep.Steps[0].Status = "passed"
	if err != nil {
		dep.Steps[0].Status, dep.Steps[0].Summary = "failed", err.Error()
	} else {
		dep.Steps[0].Summary = "deleted from " + t.Namespace
	}
	dep.mu.Unlock()
	if err != nil {
		d.finish(dep, Failed, err.Error())
		return dep, err
	}
	d.finish(dep, Removed, "")
	return dep, nil
}

// Workload is a service the factory deployed, as the cluster reports it.
type Workload struct {
	Service  string `json:"service"`
	Run      string `json:"run"`
	Image    string `json:"image"`
	Ready    int    `json:"ready"`
	Desired  int    `json:"desired"`
	Updated  int    `json:"updated"`
	Database bool   `json:"database"` // a factory-run PostgreSQL serves it
}

// TargetState is a target as its cluster reports it.
type TargetState struct {
	Target
	Reachable bool       `json:"reachable"`
	Error     string     `json:"error,omitempty"`
	Server    string     `json:"server,omitempty"` // Kubernetes version
	Workloads []Workload `json:"workloads"`
	CheckedAt time.Time  `json:"checked_at"`
}

func (d *Deployer) forget(target string) {
	d.stateMu.Lock()
	delete(d.state, target)
	d.stateMu.Unlock()
}

// State asks each target's cluster what the factory has running there.
// Answers are cached for a few seconds. The probes run on the deployer's
// own context and timeout, never the caller's: a caller that goes away
// (a reloaded page) must not cut a probe short and leave a false
// "unreachable" in the cache.
func (d *Deployer) State() []TargetState {
	out := make([]TargetState, len(d.targets))
	var wg sync.WaitGroup
	for i, t := range d.targets {
		wg.Go(func() { out[i] = d.targetState(d.ctx, t) })
	}
	wg.Wait()
	return out
}

func (d *Deployer) targetState(ctx context.Context, t Target) TargetState {
	d.stateMu.Lock()
	if s, ok := d.state[t.Name]; ok && time.Since(s.CheckedAt) < 3*time.Second {
		d.stateMu.Unlock()
		return *s
	}
	d.stateMu.Unlock()
	s := TargetState{Target: t, Workloads: []Workload{}, CheckedAt: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "--context", t.Context, "--request-timeout=5s", "version", "-o", "json")
	cmd.Env = d.baseEnv
	out, err := cmd.Output()
	var v struct {
		Server struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err != nil || json.Unmarshal(out, &v) != nil || v.Server.GitVersion == "" {
		s.Error = "cluster not reachable" + stderrOf(err)
	} else {
		s.Reachable, s.Server = true, v.Server.GitVersion
		cmd = exec.CommandContext(ctx, "kubectl", "--context", t.Context, "--request-timeout=5s", "-n", t.Namespace,
			"get", "deployments", "-l", "app.kubernetes.io/managed-by=fliessband", "-o", "json")
		cmd.Env = d.baseEnv
		if out, err := cmd.Output(); err != nil {
			s.Error = "listing deployments failed" + stderrOf(err)
		} else {
			s.Workloads = workloads(out)
		}
	}
	d.stateMu.Lock()
	d.state[t.Name] = &s
	d.stateMu.Unlock()
	return s
}

func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return ": " + lastLine(string(ee.Stderr))
	}
	if err != nil {
		return ": " + err.Error()
	}
	return ""
}

// workloads reads `kubectl get deployments -o json`.
func workloads(b []byte) []Workload {
	var list struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas   int `json:"readyReplicas"`
				UpdatedReplicas int `json:"updatedReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	out := []Workload{}
	if json.Unmarshal(b, &list) != nil {
		return out
	}
	dbs := map[string]bool{}
	for _, it := range list.Items {
		svc := it.Metadata.Labels["fliessband.dev/service"]
		if svc == "" {
			continue
		}
		if it.Metadata.Labels["fliessband.dev/component"] == "database" {
			dbs[svc] = true
			continue
		}
		w := Workload{Service: svc, Run: it.Spec.Template.Metadata.Labels["fliessband.dev/run"], Desired: 1,
			Ready: it.Status.ReadyReplicas, Updated: it.Status.UpdatedReplicas}
		if it.Spec.Replicas != nil {
			w.Desired = *it.Spec.Replicas
		}
		if cs := it.Spec.Template.Spec.Containers; len(cs) > 0 {
			w.Image = cs[0].Image
		}
		out = append(out, w)
	}
	for i := range out {
		out[i].Database = dbs[out[i].Service]
	}
	slices.SortFunc(out, func(a, b Workload) int { return strings.Compare(a.Service, b.Service) })
	return out
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// tail returns the last n lines of s.
func tail(s string, n int) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if l := strings.TrimRight(sc.Text(), " \r"); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
