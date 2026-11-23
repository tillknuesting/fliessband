package sandbox

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{Mode: Docker, Engine: "docker", Image: "golang:1.27.4", Memory: "4g", CPUs: "2",
		PIDs: 512, TmpSize: "1g", User: "501:20"}
}

func TestRunArgs(t *testing.T) {
	c := testConfig()
	c.Runtime = "runsc"
	got := c.RunArgs(Job{
		Name:   "fliessband-abc-1",
		Labels: map[string]string{"fliessband.run": "abc", "fliessband.sandbox": "1"},
		Dir:    "/data/runs/abc/service",
		Env:    []string{"HOME=/tmp", "GOPROXY=off"},
		Mounts: []Mount{
			{Path: "/data/runs/abc"},
			{Path: "/data/runs/abc/service", Writable: true},
			{Path: "/data/bridge", Target: BridgePath},
		},
		Cmd: []string{"go", "test", "./..."},
	})
	want := []string{"run", "--rm", "--name", "fliessband-abc-1",
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "501:20",
		"--memory", "4g", "--memory-swap", "4g",
		"--cpus", "2",
		"--pids-limit", "512",
		"--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=1g",
		"--init",
		"--pull", "missing", "--quiet",
		"--runtime", "runsc",
		"--label", "fliessband.run=abc", "--label", "fliessband.sandbox=1",
		"--workdir", "/data/runs/abc/service",
		"--mount", "type=bind,source=/data/runs/abc,target=/data/runs/abc,readonly",
		"--mount", "type=bind,source=/data/runs/abc/service,target=/data/runs/abc/service",
		"--mount", "type=bind,source=/data/bridge,target=/fliessband/bridge,readonly",
		"--env", "HOME=/tmp", "--env", "GOPROXY=off",
		"golang:1.27.4", "go", "test", "./...",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("docker run arguments\n got: %q\nwant: %q", got, want)
	}
}

func TestRunArgsIsolationInvariants(t *testing.T) {
	c := testConfig()
	got := c.RunArgs(Job{Name: "x", Interactive: true, Cmd: []string{"true"}})
	s := strings.Join(got, " ")
	for _, flag := range []string{"--network none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--interactive", "--rm"} {
		if !strings.Contains(s, flag) {
			t.Errorf("missing %q in %s", flag, s)
		}
	}
	if strings.Contains(s, "--runtime") || strings.Contains(s, "--privileged") || strings.Contains(s, "--publish") || strings.Contains(s, "-p ") {
		t.Errorf("unexpected flag in %s", s)
	}
	// The image comes right before the command, after every option.
	if i := slices.Index(got, "golang:1.27.4"); i != len(got)-2 || got[i+1] != "true" {
		t.Errorf("image is not followed by the command: %q", got)
	}
	// A factory-created network replaces none, for the PostgreSQL smoke test.
	n := c.RunArgs(Job{Name: "x", Network: "fliessband-net-abc"})
	if i := slices.Index(n, "--network"); n[i+1] != "fliessband-net-abc" || slices.Contains(n, "none") {
		t.Errorf("network: %q", n)
	}
}

func TestPodmanKeepsUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("keep-id is for rootless podman")
	}
	c := testConfig()
	c.Engine = "/usr/local/bin/podman"
	got := strings.Join(c.RunArgs(Job{Name: "x"}), " ")
	if !strings.Contains(got, "--userns keep-id") {
		t.Fatalf("podman without keep-id: %s", got)
	}
	if strings.Contains(strings.Join(testConfig().RunArgs(Job{Name: "x"}), " "), "keep-id") {
		t.Fatal("docker gets keep-id")
	}
}

func TestMountSpecQuotes(t *testing.T) {
	m := Mount{Path: "/a,b/c\"d", Writable: true}
	if got, want := m.spec(), `type=bind,"source=/a,b/c""d","target=/a,b/c""d"`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got := (Mount{Path: "/Application Support/x"}).spec(); got != "type=bind,source=/Application Support/x,target=/Application Support/x,readonly" {
		t.Fatalf("got %s", got)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("FLIESSBAND_SANDBOX", "")
	c, err := FromEnv("", "golang:1.27.4")
	if err != nil || c.Mode != None || c.Contained() {
		t.Fatalf("default: %+v, %v", c, err)
	}
	t.Setenv("FLIESSBAND_SANDBOX", "docker")
	t.Setenv("FLIESSBAND_SANDBOX_RUNTIME", "runsc")
	t.Setenv("FLIESSBAND_SANDBOX_ENGINE", "docker")
	t.Setenv("FLIESSBAND_SANDBOX_MEMORY", "2g")
	c, err = FromEnv("", "golang:1.27.4")
	if err != nil || !c.Contained() || c.Image != "golang:1.27.4" || c.Runtime != "runsc" || c.Memory != "2g" || c.PIDs != DefaultPIDs {
		t.Fatalf("docker: %+v, %v", c, err)
	}
	if c.User != strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()) {
		t.Fatalf("user %s", c.User)
	}
	t.Setenv("FLIESSBAND_SANDBOX_IMAGE", "example.com/go:1")
	if c, _ = FromEnv("", "golang:1.27.4"); c.Image != "example.com/go:1" {
		t.Fatalf("image %s", c.Image)
	}
	// A flag wins over the environment.
	if c, _ = FromEnv("none", "golang:1.27.4"); c.Contained() {
		t.Fatal("flag none did not win")
	}
	if _, err := FromEnv("chroot", "x"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	t.Setenv("FLIESSBAND_SANDBOX_RUNTIME", "--privileged")
	if _, err := FromEnv("docker", "x"); err == nil {
		t.Fatal("option smuggled in as runtime")
	}
}

func TestParseMemUsage(t *testing.T) {
	for in, want := range map[string]int64{
		"12.5MiB / 4GiB": 12.5 * (1 << 20),
		"1.5GiB / 4GiB":  3 << 29,
		"800kB / 1GB":    800e3,
		"512B / 1GiB":    512,
	} {
		if got, ok := ParseMemUsage(in); !ok || got != want {
			t.Errorf("%q: %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := ParseMemUsage("--"); ok {
		t.Error("parsed garbage")
	}
}

// TestBridge builds the bridge for the host and drives it through a fake
// container CLI, so the relay and the dialer are tested without Docker.
func TestBridge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the fake CLI")
	}
	dir := t.TempDir()
	bin, err := buildBridge(context.Background(), dir, runtime.GOOS, runtime.GOARCH, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	// docker exec --interactive NAME /fliessband/bridge ADDR -> bridge ADDR
	engine := filepath.Join(dir, "fake-docker")
	script := "#!/bin/sh\n[ \"$1\" = exec ] || exit 9\nexec '" + bin + "' \"$5\"\n"
	if err := os.WriteFile(engine, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	c := testConfig()
	c.Engine, c.EnvCLI = engine, os.Environ()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := c.DialBridge(ctx, "srv", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello through the sandbox bridge")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != string(msg) {
		t.Fatalf("echo %q, %v", got, err)
	}
	_ = conn.Close()

	// Nothing listens: the dial fails instead of returning a dead connection.
	_ = l.Close()
	if _, err := c.DialBridge(ctx, "srv", l.Addr().String()); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	// The cached binary is reused.
	again, err := buildBridge(ctx, dir, runtime.GOOS, runtime.GOARCH, os.Environ())
	if err != nil || again != bin {
		t.Fatalf("rebuild: %s, %v", again, err)
	}
}

// TestDockerContainer runs a real container when a Docker daemon answers.
func TestDockerContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker info fails: no container engine")
	}
	c, err := FromEnv("docker", "golang:1.27.4")
	if err != nil {
		t.Fatal(err)
	}
	c.EnvCLI = os.Environ()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := c.Command(ctx, Job{
		Name:   "fliessband-sandbox-test-" + strconv.Itoa(os.Getpid()),
		Dir:    dir,
		Env:    []string{"HOME=/tmp", "PATH=/usr/local/go/bin:/usr/bin:/bin"},
		Mounts: []Mount{{Path: dir, Writable: true}},
		Cmd: []string{"sh", "-c", `touch /rootfs-write 2>/dev/null && echo rootfs-writable; ls /sys/class/net | tr '\n' ' '; echo; id -u;
			touch ./ok && echo dir-writable; go version`},
	}).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	s := string(out)
	if strings.Contains(s, "rootfs-writable") || !strings.Contains(s, "dir-writable") || !strings.Contains(s, "go version") {
		t.Fatalf("isolation: %s", s)
	}
	if nets := strings.Fields(strings.Split(s, "\n")[0]); len(nets) != 1 || nets[0] != "lo" {
		t.Fatalf("network interfaces %v, want only lo", nets)
	}
	if !strings.Contains(s, "\n"+strconv.Itoa(os.Getuid())+"\n") {
		t.Fatalf("not running as uid %d: %s", os.Getuid(), s)
	}
}
