package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/mutate"
	"github.com/tillknuesting/fliessband/internal/sandbox"
)

// Which steps execute generated code, and so run in the sandbox:
//
//   - go test: the test and acceptance stations, LLM test hardening,
//     tournament candidate tests, mutants (go test -overlay) and fuzzing;
//   - the built server in the smoke station.
//
// Compiling (build, vet, lint, go fix, govulncheck, buf) does not execute
// the code: go generate would, and the policy forbids its directives. The
// dependency station needs the network to download modules and executes
// nothing. The PostgreSQL integration tests run on the host because they
// must reach the database; they compile only the store package, which the
// templates own and which does not link the LLM's service code.

// DefaultSandboxImage is the official Go image for the toolchain that
// generated modules pin.
func DefaultSandboxImage() string { return "golang:" + strings.TrimPrefix(gen.Toolchain, "go") }

// sandboxPATH is the PATH in the official golang image.
const sandboxPATH = "/usr/local/go/bin:/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// SetSandbox configures how generated code is executed. A container
// sandbox must be reachable now: the factory does not fall back to running
// code unsandboxed. Call it before starting runs.
func (m *Manager) SetSandbox(ctx context.Context, c sandbox.Config) error {
	if !c.Contained() {
		m.sandbox = sandbox.Config{Mode: sandbox.None}
		return nil
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if c.Podman() {
		for _, k := range []string{"XDG_RUNTIME_DIR", "CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	} else {
		d := m.docker()
		if !d.ok {
			return fmt.Errorf("sandbox docker: %s", d.reason)
		}
		env = append(env, "DOCKER_CONFIG="+d.config) // no registry credentials
		if d.host != "" {
			env = append(env, "DOCKER_HOST="+d.host)
		}
	}
	c.EnvCLI = env
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := c.Check(cctx); err != nil {
		return err
	}
	root := filepath.Join(m.dataDir, "sandbox")
	for _, d := range []string{"gocache", "gomodcache"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			return err
		}
	}
	// Execution steps get the factory's own module cache, read-only, and a
	// build cache of their own: code in the container can never write into
	// a cache that host builds read.
	if os.Getenv("GOMODCACHE") == "" {
		m.goPaths["GOMODCACHE"] = filepath.Join(root, "gomodcache")
	}
	m.sandbox = c
	m.sandboxArch = c.Arch(cctx)
	return nil
}

// Sandbox describes how generated code is executed, for /api/status and
// run reports.
func (m *Manager) Sandbox() *metrics.Sandbox {
	c := m.sandbox
	if !c.Contained() {
		return &metrics.Sandbox{Mode: string(sandbox.None), Detail: c.String()}
	}
	return &metrics.Sandbox{Mode: string(c.Mode), Contained: true, Engine: filepath.Base(c.Engine), Image: c.Image,
		Runtime: c.Runtime, Network: "none", Detail: c.String()}
}

func (m *Manager) sandboxDir(sub string) string { return filepath.Join(m.dataDir, "sandbox", sub) }

func (m *Manager) modCache() string {
	if v := os.Getenv("GOMODCACHE"); v != "" {
		return v
	}
	return m.goPaths["GOMODCACHE"]
}

// executesCode reports whether a tool invocation runs generated code.
func executesCode(name string, args []string) bool {
	if name != "go" || len(args) == 0 {
		return false
	}
	switch args[0] {
	case "test", "run", "generate":
		return true
	}
	return false
}

// newCommand builds the process for x.command: in the sandbox when the
// command executes generated code and a sandbox is configured, else on the
// host in its own process group with the scrubbed environment. It returns
// the command line to log.
func (x *executor) newCommand(ctx context.Context, o cmdOpts, path, name string, args []string) (*exec.Cmd, string) {
	dir := x.r.ServiceDir()
	if o.dir != "" {
		dir = o.dir
	}
	shown := name + " " + strings.Join(args, " ")
	if x.m.sandbox.Contained() && !o.host && executesCode(name, args) {
		return x.sandboxCommand(ctx, dir, o.env, append([]string{name}, args...)), "[sandbox] " + shown
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = x.m.childEnv(o.network, o.env...)
	isolate(cmd)
	return cmd, shown
}

var sandboxSeq atomic.Int64

func (x *executor) containerName(kind string) string {
	return fmt.Sprintf("fliessband-%s-%s-%d", x.r.ID, kind, sandboxSeq.Add(1))
}

// realPath resolves symlinks, so the container sees the same paths the go
// command reports on the host (macOS keeps temp dirs behind /var ->
// /private/var).
func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// pathMounts mounts p at its real path and, when that differs, also at p.
func pathMounts(p string, writable bool) []sandbox.Mount {
	abs, _ := filepath.Abs(p)
	real := realPath(p)
	out := []sandbox.Mount{{Path: real, Writable: writable}}
	if abs != real {
		out = append(out, sandbox.Mount{Path: real, Target: abs, Writable: writable})
	}
	return out
}

// goJobEnv is the environment of the go command in the container: offline,
// with the factory's caches, and nothing from the host's environment.
func (x *executor) goJobEnv(extra []string) []string {
	env := []string{
		"PATH=" + sandboxPATH, "HOME=/tmp", "TMPDIR=/tmp", "GOPATH=/tmp/go",
		"GOCACHE=" + realPath(x.m.sandboxDir("gocache")), "GOMODCACHE=" + realPath(x.m.modCache()),
		"GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "GOFLAGS=-mod=readonly", "CGO_ENABLED=1",
	}
	return append(env, extra...)
}

// goJob is a go command in the container. The run directory is mounted
// read-only (overlays and tournament candidates live there), the service
// directory read-write, the module cache read-only and the sandbox build
// cache read-write. Coverage profiles written outside the service
// directory are mounted as single writable files.
func (x *executor) goJob(dir string, env, cmd []string) sandbox.Job {
	svc := x.r.ServiceDir()
	mounts := pathMounts(x.r.Dir(), false)
	mounts = append(mounts, pathMounts(svc, true)...)
	mounts = append(mounts, pathMounts(x.m.modCache(), false)...)
	mounts = append(mounts, pathMounts(x.m.sandboxDir("gocache"), true)...)
	for _, a := range cmd {
		p, ok := strings.CutPrefix(a, "-coverprofile=")
		if !ok || within(p, svc) {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			_ = os.WriteFile(p, nil, 0o600) // a bind mount needs an existing file
		}
		mounts = append(mounts, pathMounts(p, true)...)
	}
	return sandbox.Job{
		Name:   x.containerName("go"),
		Labels: map[string]string{"fliessband.run": x.r.ID},
		Dir:    realPath(dir),
		Env:    x.goJobEnv(env),
		Mounts: mounts,
		Cmd:    cmd,
	}
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sandboxCommand runs a go command in the container.
func (x *executor) sandboxCommand(ctx context.Context, dir string, env, cmd []string) *exec.Cmd {
	return x.m.sandbox.Command(ctx, x.goJob(dir, env, cmd))
}

// sandboxMutations makes the mutation runner execute mutants in the
// sandbox. Mutant sources and overlays go under the run directory, which
// the container can read.
func (x *executor) sandboxMutations(r *mutate.Runner) {
	if !x.m.sandbox.Contained() {
		return
	}
	tmp := filepath.Join(x.r.Dir(), "_mutants")
	if err := os.MkdirAll(tmp, 0o750); err == nil {
		r.TempDir = realPath(tmp)
	}
	r.Command = func(ctx context.Context, dir string, args []string) *exec.Cmd {
		return x.sandboxCommand(ctx, dir, nil, append([]string{"go"}, args...))
	}
}
