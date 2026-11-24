package deploy

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseTargets(t *testing.T) {
	ts, err := ParseTargets(" dev=k3d-fliessband, ci=kind-ci/services ,prod=gke_acme_eu/inventory@ghcr.io/acme/")
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{
		{Name: "dev", Context: "k3d-fliessband", Namespace: "fliessband", Strategy: K3d, Cluster: "fliessband", Local: true},
		{Name: "ci", Context: "kind-ci", Namespace: "services", Strategy: Kind, Cluster: "ci", Local: true},
		{Name: "prod", Context: "gke_acme_eu", Namespace: "inventory", Strategy: Registry, Registry: "ghcr.io/acme/"},
	}
	if len(ts) != len(want) {
		t.Fatalf("got %d targets, want %d", len(ts), len(want))
	}
	for i := range want {
		if ts[i] != want[i] {
			t.Errorf("target %d = %+v, want %+v", i, ts[i], want[i])
		}
	}
	for _, bad := range []string{
		"prod=gke_acme_eu",     // remote without a registry
		"Dev=k3d-x",            // upper case name
		"dev=k3d-x,dev=kind-y", // twice
		"dev=",                 // no context
		"dev=k3d-x/Not_Valid",  // namespace
		"just-a-context-no-eq", // no name
	} {
		if _, err := ParseTargets(bad); err == nil {
			t.Errorf("ParseTargets(%q) succeeded", bad)
		}
	}
	if ts, err := ParseTargets(""); err != nil || len(ts) != 0 {
		t.Errorf("empty = %v, %v", ts, err)
	}
}

func TestDetect(t *testing.T) {
	for ctx, want := range map[string]Strategy{"k3d-a": K3d, "kind-b": Kind, "minikube": Minikube, "docker-desktop": Shared,
		"orbstack": Shared, "colima": Shared, "rancher-desktop": Shared, "arn:aws:eks:x": Registry} {
		if s, _, local := detect(ctx); s != want || local != (want != Registry) {
			t.Errorf("detect(%q) = %s, local %v", ctx, s, local)
		}
	}
}

func TestKustomization(t *testing.T) {
	j := &job{t: Target{Namespace: "shop", Strategy: K3d}, svc: Service{Name: "inventory", Run: "20261124-151322-aec4"}}
	app := j.kustomization([]string{"../base"}, "", true)
	for _, want := range []string{"namespace: shop", "  - ../base", "app.kubernetes.io/managed-by: fliessband",
		"fliessband.dev/service: inventory", `fliessband.dev/run: "20261124-151322-aec4"`, "includeTemplates: true",
		"  - name: inventory\n    newName: fliessband/inventory\n    newTag: \"20261124-151322-aec4\""} {
		if !strings.Contains(app, want) {
			t.Errorf("app overlay lacks %q:\n%s", want, app)
		}
	}
	if strings.Count(app, "includeSelectors: false") != 2 {
		t.Errorf("labels must never enter selectors, which are immutable:\n%s", app)
	}
	db := j.kustomization([]string{"database.yaml"}, "database", false)
	if strings.Contains(db, "fliessband.dev/run") || strings.Contains(db, "images:") {
		t.Errorf("the database overlay must not change with each version, or its pod restarts and loses its data:\n%s", db)
	}
	if !strings.Contains(db, "fliessband.dev/component: database") {
		t.Errorf("database overlay lacks its component label:\n%s", db)
	}
	j.t = Target{Namespace: "shop", Strategy: Registry, Registry: "ghcr.io/acme/"}
	if got := j.imageRef(); got != "ghcr.io/acme/inventory:20261124-151322-aec4" {
		t.Errorf("registry image = %s", got)
	}
}

func TestRolloutProgress(t *testing.T) {
	const run = "r2"
	dep := func(gen, observed int64, label string, replicas, updated, available int) []byte {
		return []byte(strings.NewReplacer("GEN", itoa(gen), "OBS", itoa(observed), "RUN", label, "REP", itoa(int64(replicas)),
			"UPD", itoa(int64(updated)), "AVA", itoa(int64(available))).Replace(
			`{"metadata":{"generation":GEN},"spec":{"replicas":2,"template":{"metadata":{"labels":{"fliessband.dev/run":"RUN"}}}},
			"status":{"observedGeneration":OBS,"replicas":REP,"updatedReplicas":UPD,"readyReplicas":AVA,"availableReplicas":AVA}}`))
	}
	for _, c := range []struct {
		name string
		b    []byte
		done bool
	}{
		{"done", dep(3, 3, run, 2, 2, 2), true},
		{"controller has not seen the change", dep(3, 2, run, 2, 2, 2), false},
		{"old pod still running", dep(3, 3, run, 3, 2, 2), false},
		{"not available", dep(3, 3, run, 2, 2, 1), false},
		{"other version", dep(3, 3, "r1", 2, 2, 2), false},
	} {
		if p := rolloutProgress(c.b, run); p.done != c.done {
			t.Errorf("%s: done = %v (%s)", c.name, p.done, p.line)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestWorstPod(t *testing.T) {
	b := []byte(`{"items":[
		{"status":{"containerStatuses":[{"restartCount":0,"state":{"waiting":{"reason":"ContainerCreating"}}}]}},
		{"status":{"containerStatuses":[{"restartCount":3,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
		{"status":{"containerStatuses":[{"restartCount":0,"state":{"running":{}}}]}}]}`)
	reason, restarts, err := worstPod(b)
	if err != nil || reason != "CrashLoopBackOff" || restarts != 3 {
		t.Errorf("worstPod = %q, %d, %v", reason, restarts, err)
	}
	reason, _, _ = worstPod([]byte(`{"items":[{"status":{"containerStatuses":[{"restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}},
		{"restartCount":0,"state":{"waiting":{"reason":"CreateContainerConfigError"}}}]}}]}`))
	if reason != "CreateContainerConfigError" {
		t.Errorf("a fatal reason must win over restarts, got %q", reason)
	}
}

func TestWorkloads(t *testing.T) {
	b := []byte(`{"items":[
		{"metadata":{"labels":{"fliessband.dev/service":"inventory"}},"spec":{"replicas":2,"template":{"metadata":{"labels":{"fliessband.dev/run":"r7"}},
		 "spec":{"containers":[{"image":"fliessband/inventory:r7"}]}}},"status":{"readyReplicas":2,"updatedReplicas":2}},
		{"metadata":{"labels":{"fliessband.dev/service":"inventory","fliessband.dev/component":"database"}},"spec":{"replicas":1,"template":{"metadata":{},"spec":{"containers":[{"image":"postgres:17-alpine"}]}}},"status":{}},
		{"metadata":{"labels":{}},"spec":{"template":{"metadata":{},"spec":{"containers":[]}}},"status":{}}]}`)
	ws := workloads(b)
	if len(ws) != 1 {
		t.Fatalf("workloads = %+v", ws)
	}
	w := ws[0]
	if w.Service != "inventory" || w.Run != "r7" || w.Ready != 2 || w.Desired != 2 || !w.Database || w.Image != "fliessband/inventory:r7" {
		t.Errorf("workload = %+v", w)
	}
}

func TestDeployerHistory(t *testing.T) {
	d, err := New(t.TempDir(), []Target{{Name: "dev", Context: "k3d-dev", Namespace: "fliessband", Strategy: K3d}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, run string) *Deployment {
		dep := &Deployment{ID: id, Action: "deploy", Target: "dev", Service: "inventory", Run: run, Status: Running, CreatedAt: time.Now()}
		d.all[id], d.own[id] = dep, true
		return dep
	}
	a := mk("a", "r1")
	d.finish(a, Live, "")
	b := mk("b", "r2")
	d.finish(b, Live, "")
	if a.Status != Replaced || d.Live("dev", "inventory").ID != "b" {
		t.Fatalf("after a second version went live: a %s, live %v", a.Status, d.Live("dev", "inventory"))
	}
	c := mk("c", "r3")
	d.finish(c, RolledBack, "CrashLoopBackOff")
	if d.Live("dev", "inventory").ID != "b" {
		t.Fatal("a rolled back version must leave the previous one live")
	}
	// A restart reads the history back; a deployment cut off mid-way is failed.
	e := mk("e", "r4")
	e.PID = 1 << 30 // no such process
	_ = d.save(e)
	d2, err := New(strings.TrimSuffix(d.dir, "/deploys"), d.targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d2.Live("dev", "inventory"); got == nil || got.Run != "r2" {
		t.Fatalf("live after reload = %+v", got)
	}
	if e := d2.Get("e"); e == nil || e.Status != Failed || !strings.Contains(e.Error, "interrupted") {
		t.Fatalf("interrupted deployment = %+v", e)
	}
	if _, err := d.Deploy("nope", Service{Name: "x"}); err == nil {
		t.Fatal("deploying to an unknown target succeeded")
	}
}

// A deployment made by another process (fliessband deploy in a terminal)
// shows up without a restart, and replaces what was live.
func TestDeployerSeesOtherProcess(t *testing.T) {
	dir := t.TempDir()
	targets := []Target{{Name: "dev", Context: "k3d-dev", Namespace: "fliessband", Strategy: K3d}}
	server, err := New(dir, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := New(dir, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &Deployment{ID: "a", Action: "deploy", Target: "dev", Service: "inventory", Run: "r1", Status: Running, CreatedAt: time.Now()}
	server.mu.Lock()
	server.all["a"], server.own["a"] = a, true
	server.mu.Unlock()
	server.finish(a, Live, "")

	b := &Deployment{ID: "b", Action: "deploy", Target: "dev", Service: "inventory", Run: "r2", Status: Running, CreatedAt: time.Now(), PID: os.Getpid()}
	cli.mu.Lock()
	cli.rescanLocked()
	cli.all["b"], cli.own["b"] = b, true
	if !cli.runningLocked("dev", "inventory") {
		t.Error("the CLI's own running deployment does not count as running")
	}
	cli.mu.Unlock()
	_ = cli.save(b)
	if server.Get("b") == nil {
		t.Fatal("the server does not see the CLI's deployment")
	}
	server.mu.Lock()
	busy := server.runningLocked("dev", "inventory")
	server.mu.Unlock()
	if !busy {
		t.Error("the server would start a second deployment while the CLI's runs")
	}
	time.Sleep(10 * time.Millisecond) // a newer file time than the first save
	cli.finish(b, Live, "")
	if live := server.Live("dev", "inventory"); live == nil || live.Run != "r2" {
		t.Fatalf("server's live version = %+v, want r2", live)
	}
}
