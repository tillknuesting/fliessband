package pipeline

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tillknuesting/fliessband/internal/sandbox"
)

// smokeServer is the built server as the smoke station drives it: a host
// process, or a container without a network that the client reaches
// through the sandbox bridge.
type smokeServer struct {
	cmd     *exec.Cmd
	dial    func(ctx context.Context, addr string) (net.Conn, error)
	term    func() error         // ask for a graceful shutdown
	kill    func()               // stop it now
	rss     func() (int64, bool) // resident memory
	cleanup func()
	where   string // for the log
}

// smokeServer runs the warm-up and returns the server to start. env
// is the server's configuration (GRPC_ADDR, ...).
func (x *executor) smokeServer(ctx context.Context, st *StageState, env []string) (*smokeServer, time.Duration, error) {
	if !x.m.sandbox.Contained() {
		return x.hostServer(env), x.hostWarmup(), nil
	}
	return x.sandboxServer(ctx, st, env)
}

// hostWarmup pays for the OS check of a new binary with a run that exits
// at once on bad config.
func (x *executor) hostWarmup() time.Duration {
	warm := exec.Command(x.binary())
	warm.Env = x.m.childEnv(false, "LOG_LEVEL=not-a-level")
	t0 := time.Now()
	_ = warm.Run()
	return time.Since(t0)
}

func (x *executor) hostServer(env []string) *smokeServer {
	cmd := exec.Command(x.binary())
	cmd.Env = x.m.childEnv(false, env...) // the binary contains LLM-written code
	s := &smokeServer{cmd: cmd, where: "on the host", cleanup: func() {}}
	s.term = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	s.kill = func() { _ = cmd.Process.Kill() }
	s.rss = func() (int64, bool) {
		b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(cmd.Process.Pid)).Output()
		if err != nil {
			return 0, false
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return kb * 1024, err == nil
	}
	return s
}

// sandboxServer cross-compiles the server for the sandbox's Linux host
// (compiling runs no generated code) and prepares a container for it with
// no network. With PostgreSQL, the container joins an internal network
// that holds only the database instead.
func (x *executor) sandboxServer(ctx context.Context, st *StageState, env []string) (*smokeServer, time.Duration, error) {
	c := x.m.sandbox
	arch := x.m.sandboxArch
	bin := filepath.Join(x.r.Dir(), "bin", "server-linux-"+arch)
	res, err := x.command(ctx, st, cmdOpts{env: []string{"GOOS=linux", "GOARCH=" + arch, "CGO_ENABLED=0"}},
		"go", "build", "-trimpath", "-buildvcs=false", "-o", bin, "./cmd/server")
	if err != nil {
		return nil, 0, err
	}
	if res.code != 0 {
		return nil, 0, fmt.Errorf("cross-compiling the server for the sandbox failed: %s", lastLine(res.stderr))
	}
	bridge, err := sandbox.BuildBridge(ctx, x.m.sandboxDir("bridge"), arch, x.m.childEnv(false))
	if err != nil {
		return nil, 0, err
	}
	const serverPath = "/fliessband/server"
	job := func(kind string, env []string) sandbox.Job {
		return sandbox.Job{
			Name:   x.containerName(kind),
			Labels: map[string]string{"fliessband.run": x.r.ID},
			Dir:    "/tmp",
			Env:    append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp"}, env...),
			Mounts: []sandbox.Mount{{Path: realPath(bin), Target: serverPath}, {Path: realPath(bridge), Target: sandbox.BridgePath}},
			Cmd:    []string{serverPath},
		}
	}
	t0 := time.Now()
	_ = c.Command(ctx, job("warm", []string{"LOG_LEVEL=not-a-level"})).Run()
	warm := time.Since(t0)

	s := &smokeServer{where: "in the sandbox (" + filepath.Base(c.Engine) + ", no network)", cleanup: func() {}}
	j := job("smoke", env)
	if x.db != nil {
		network, url, cleanup, err := x.sandboxDBNetwork(ctx)
		if err != nil {
			return nil, 0, err
		}
		s.cleanup = cleanup
		j.Network = network
		j.Env = append(j.Env, "DATABASE_URL="+url) // the last value wins
		s.where = "in the sandbox (" + filepath.Base(c.Engine) + ", internal network with only PostgreSQL)"
	}
	s.cmd = c.Command(ctx, j)
	s.dial = func(ctx context.Context, addr string) (net.Conn, error) { return c.DialBridge(ctx, j.Name, addr) }
	s.term = func() error {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return c.CLI(cctx, "kill", "--signal", "TERM", j.Name).Run()
	}
	s.kill = func() { c.Remove(j.Name) }
	s.rss = func() (int64, bool) {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := c.CLI(cctx, "stats", "--no-stream", "--format", "{{.MemUsage}}", j.Name).Output()
		if err != nil {
			return 0, false
		}
		return sandbox.ParseMemUsage(string(out))
	}
	return s, warm, nil
}

// sandboxDBNetwork creates an internal network (no route out), attaches the
// PostgreSQL container to it as "db", and returns the network, the URL the
// server uses, and a cleanup function.
func (x *executor) sandboxDBNetwork(ctx context.Context) (string, string, func(), error) {
	c := x.m.sandbox
	network := "fliessband-net-" + x.r.ID
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if out, err := c.CLI(cctx, "network", "create", "--internal", "--label", "fliessband.run="+x.r.ID, network).CombinedOutput(); err != nil {
		return "", "", nil, fmt.Errorf("create the sandbox network: %s", lastLine(string(out)))
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.CLI(ctx, "network", "disconnect", "--force", network, x.db.container).Run()
		_ = c.CLI(ctx, "network", "rm", network).Run()
	}
	if out, err := c.CLI(cctx, "network", "connect", "--alias", "db", network, x.db.container).CombinedOutput(); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("attach PostgreSQL to the sandbox network (the sandbox engine must also run PostgreSQL): %s", lastLine(string(out)))
	}
	return network, "postgres://postgres:fliessband@db:5432/postgres?sslmode=disable", cleanup, nil
}
