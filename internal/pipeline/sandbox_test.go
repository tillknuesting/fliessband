package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/mutate"
	"github.com/tillknuesting/fliessband/internal/sandbox"
)

func sandboxedExecutor(t *testing.T) *executor {
	t.Helper()
	data := t.TempDir()
	runDir := filepath.Join(data, "runs", "r1")
	if err := os.MkdirAll(filepath.Join(runDir, "service"), 0o750); err != nil {
		t.Fatal(err)
	}
	m := &Manager{dataDir: data, goPaths: map[string]string{"GOMODCACHE": filepath.Join(data, "sandbox", "gomodcache")}}
	m.sandbox = sandbox.Config{Mode: sandbox.Docker, Engine: "docker", Image: "golang:1.27.4", Memory: "4g", CPUs: "2",
		PIDs: 1024, TmpSize: "1g", User: "501:20"}
	t.Setenv("GOMODCACHE", "")
	return &executor{m: m, r: &Run{ID: "r1", dir: runDir}}
}

func TestExecutesCode(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want bool
	}{
		{"go", []string{"test", "./..."}, true},
		{"go", []string{"run", "./cmd/server"}, true},
		{"go", []string{"generate", "./..."}, true},
		{"go", []string{"build", "./..."}, false},
		{"go", []string{"vet", "./..."}, false},
		{"go", []string{"mod", "tidy"}, false},
		{"golangci-lint", []string{"run"}, false},
		{"govulncheck", []string{"./..."}, false},
		{"buf", []string{"generate"}, false},
	} {
		if got := executesCode(c.name, c.args); got != c.want {
			t.Errorf("%s %v: %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

// The test station's command runs in a container with no network, the run
// directory read-only, the service directory and the coverage file
// writable, and only offline Go settings.
func TestSandboxedTestCommand(t *testing.T) {
	x := sandboxedExecutor(t)
	cover := x.coverPath()
	cmd, shown := x.newCommand(context.Background(), cmdOpts{env: []string{"EXTRA=1"}}, "/usr/local/go/bin/go", "go",
		[]string{"test", "-race", "-coverprofile=" + cover, "./..."})
	if !strings.HasPrefix(shown, "[sandbox] go test") {
		t.Fatalf("not shown as sandboxed: %s", shown)
	}
	if filepath.Base(cmd.Path) != "docker" && cmd.Args[0] != "docker" {
		t.Fatalf("not run through docker: %v", cmd.Args)
	}
	args := cmd.Args[1:]
	joined := strings.Join(args, " ")
	for _, want := range []string{"run --rm", "--network none", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--user 501:20", "--pids-limit 1024", "--label fliessband.run=r1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q", want)
		}
	}
	mounts := map[string]string{}
	for i, a := range args {
		if a == "--mount" {
			spec := args[i+1]
			var src string
			for _, f := range strings.Split(spec, ",") {
				if s, ok := strings.CutPrefix(f, "target="); ok {
					src = s
				}
			}
			mounts[src] = spec
		}
	}
	real := func(p string) string { return realPath(p) }
	check := func(path string, writable bool) {
		t.Helper()
		spec, ok := mounts[real(path)]
		if !ok {
			t.Fatalf("%s is not mounted: %v", path, mounts)
		}
		if ro := strings.HasSuffix(spec, ",readonly"); ro == writable {
			t.Errorf("%s writable=%v, mount %s", path, writable, spec)
		}
	}
	check(x.r.Dir(), false)
	check(x.r.ServiceDir(), true)
	check(cover, true)
	check(x.m.modCache(), false)
	check(x.m.sandboxDir("gocache"), true)
	if len(mounts) > 5+5 { // each path at most twice (real and given path)
		t.Errorf("unexpected mounts: %v", mounts)
	}
	if _, err := os.Stat(cover); err != nil {
		t.Errorf("coverage file not created for its bind mount: %v", err)
	}
	var env []string
	for i, a := range args {
		if a == "--env" {
			env = append(env, args[i+1])
		}
	}
	for _, want := range []string{"GOPROXY=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local", "HOME=/tmp", "EXTRA=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("container env misses %s: %v", want, env)
		}
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if isSecretName(k) {
			t.Errorf("credential-like variable %s passed into the sandbox", k)
		}
	}
	if i := slices.Index(args, "golang:1.27.4"); i < 0 || !slices.Equal(args[i+1:], []string{"go", "test", "-race", "-coverprofile=" + cover, "./..."}) {
		t.Errorf("command after the image: %v", args)
	}
	if i := slices.Index(args, "--workdir"); i < 0 || args[i+1] != realPath(x.r.ServiceDir()) {
		t.Errorf("workdir: %v", args)
	}
}

func TestSandboxLeavesCompilingAndIntegrationOnHost(t *testing.T) {
	x := sandboxedExecutor(t)
	for _, c := range []struct {
		o    cmdOpts
		args []string
	}{
		{cmdOpts{}, []string{"build", "./..."}},
		{cmdOpts{}, []string{"vet", "./..."}},
		{cmdOpts{network: true}, []string{"mod", "tidy"}},
		{cmdOpts{host: true}, []string{"test", "-run", "Postgres", "./internal/store/"}},
	} {
		cmd, shown := x.newCommand(context.Background(), c.o, "/usr/local/go/bin/go", "go", c.args)
		if strings.Contains(shown, "sandbox") || cmd.Path != "/usr/local/go/bin/go" {
			t.Errorf("go %v ran in the sandbox: %v", c.args, cmd.Args)
		}
	}
}

func TestNoSandboxRunsOnHost(t *testing.T) {
	x := sandboxedExecutor(t)
	x.m.sandbox = sandbox.Config{}
	cmd, shown := x.newCommand(context.Background(), cmdOpts{}, "/usr/local/go/bin/go", "go", []string{"test", "./..."})
	if strings.Contains(shown, "sandbox") || cmd.Path != "/usr/local/go/bin/go" {
		t.Fatalf("no sandbox configured but: %v", cmd.Args)
	}
	if s := x.m.Sandbox(); s.Mode != "none" || s.Contained {
		t.Fatalf("status %+v", s)
	}
}

func TestSandboxedMutants(t *testing.T) {
	x := sandboxedExecutor(t)
	r := &mutate.Runner{Root: x.r.ServiceDir()}
	x.sandboxMutations(r)
	if r.Command == nil || !within(r.TempDir, realPath(x.r.Dir())) {
		t.Fatalf("mutants not sandboxed: temp %q", r.TempDir)
	}
	cmd := r.Command(context.Background(), realPath(x.r.ServiceDir()), []string{"test", "-overlay=" + filepath.Join(r.TempDir, "m.json"), "./internal/service"})
	if !slices.Contains(cmd.Args, "none") || !slices.Contains(cmd.Args, "go") {
		t.Fatalf("mutant command: %v", cmd.Args)
	}
	if s := x.m.Sandbox(); s.Mode != "docker" || !s.Contained || s.Network != "none" || s.Image != "golang:1.27.4" {
		t.Fatalf("status %+v", s)
	}
}

func TestDefaultSandboxImage(t *testing.T) {
	if got := DefaultSandboxImage(); !strings.HasPrefix(got, "golang:1.") || strings.Contains(got, "go1") {
		t.Fatalf("default image %s", got)
	}
}
