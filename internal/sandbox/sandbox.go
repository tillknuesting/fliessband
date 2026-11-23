// Package sandbox runs the commands that execute generated code (tests,
// fuzzing, mutants, the built server) either as ordinary child processes
// ("none") or in a locked-down container ("docker", which also works with
// the Podman CLI).
//
// A container gets no network, a read-only root filesystem, no Linux
// capabilities, no privilege escalation, a non-root user, memory, CPU and
// process limits, and only the directories a step needs, mounted at the same
// absolute paths as on the host so that file names in arguments, overlays
// and tool output stay valid.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mode selects how generated code is executed.
type Mode string

const (
	// None runs commands as ordinary child processes of the factory.
	None Mode = "none"
	// Docker runs commands in a container through the Docker or Podman CLI.
	Docker Mode = "docker"
)

// ParseMode accepts "", "none", "off", "docker" and "podman" (an alias of
// docker that prefers the podman CLI).
func ParseMode(s string) (Mode, string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "off":
		return None, "", nil
	case "docker":
		return Docker, "", nil
	case "podman":
		return Docker, "podman", nil
	}
	return "", "", fmt.Errorf("unknown sandbox %q: use none or docker", s)
}

// Config describes the sandbox. The zero value is "none".
type Config struct {
	Mode    Mode
	Engine  string   // container CLI: docker or podman (name or path)
	Image   string   // image with the Go toolchain, e.g. golang:1.27.4
	Runtime string   // optional OCI runtime, e.g. runsc (gVisor)
	Memory  string   // memory limit, e.g. 4g (swap is capped to the same)
	CPUs    string   // CPU limit, e.g. 4
	PIDs    int      // process and thread limit
	TmpSize string   // size of the writable tmpfs at /tmp
	User    string   // uid:gid the commands run as
	EnvCLI  []string // environment of the container CLI itself (DOCKER_HOST, ...)
}

// Default resource limits.
const (
	DefaultMemory  = "4g"
	DefaultPIDs    = 1024
	DefaultTmpSize = "1g"
)

// FromEnv reads FLIESSBAND_SANDBOX, FLIESSBAND_SANDBOX_IMAGE,
// FLIESSBAND_SANDBOX_RUNTIME, FLIESSBAND_SANDBOX_ENGINE,
// FLIESSBAND_SANDBOX_MEMORY and FLIESSBAND_SANDBOX_CPUS. mode, when not
// empty, overrides FLIESSBAND_SANDBOX (a command-line flag). defaultImage is
// used when no image is configured.
func FromEnv(mode, defaultImage string) (Config, error) {
	if mode == "" {
		mode = os.Getenv("FLIESSBAND_SANDBOX")
	}
	m, engine, err := ParseMode(mode)
	if err != nil {
		return Config{}, err
	}
	c := Config{Mode: m}
	if m == None {
		return c, nil
	}
	c.Engine = firstNonEmpty(os.Getenv("FLIESSBAND_SANDBOX_ENGINE"), engine)
	c.Image = firstNonEmpty(os.Getenv("FLIESSBAND_SANDBOX_IMAGE"), defaultImage)
	c.Runtime = os.Getenv("FLIESSBAND_SANDBOX_RUNTIME")
	c.Memory = os.Getenv("FLIESSBAND_SANDBOX_MEMORY")
	c.CPUs = os.Getenv("FLIESSBAND_SANDBOX_CPUS")
	return c.withDefaults(), c.validate()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// withDefaults fills unset fields.
func (c Config) withDefaults() Config {
	if c.Mode == "" {
		c.Mode = None
	}
	if c.Mode == None {
		return c
	}
	if c.Engine == "" {
		c.Engine = "docker"
		if _, err := exec.LookPath("docker"); err != nil {
			if _, err := exec.LookPath("podman"); err == nil {
				c.Engine = "podman"
			}
		}
	}
	if c.Memory == "" {
		c.Memory = DefaultMemory
	}
	if c.CPUs == "" {
		c.CPUs = strconv.Itoa(min(runtime.NumCPU(), 4))
	}
	if c.PIDs == 0 {
		c.PIDs = DefaultPIDs
	}
	if c.TmpSize == "" {
		c.TmpSize = DefaultTmpSize
	}
	if c.User == "" {
		c.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	return c
}

// validate rejects values that would be read as extra CLI options.
func (c Config) validate() error {
	for name, v := range map[string]string{"image": c.Image, "runtime": c.Runtime, "memory": c.Memory, "cpus": c.CPUs} {
		if strings.HasPrefix(v, "-") || strings.ContainsAny(v, " \t\n") {
			return fmt.Errorf("sandbox %s %q is not valid", name, v)
		}
	}
	if c.Mode == Docker && c.Image == "" {
		return errors.New("the docker sandbox needs an image (FLIESSBAND_SANDBOX_IMAGE)")
	}
	return nil
}

// Contained reports whether commands run in a sandbox.
func (c Config) Contained() bool { return c.Mode == Docker }

// String describes the sandbox for logs.
func (c Config) String() string {
	if !c.Contained() {
		return "none (generated code runs as ordinary processes on this host)"
	}
	s := fmt.Sprintf("%s container from %s, no network, read-only root, all capabilities dropped, user %s, %s memory, %s CPUs",
		filepath.Base(c.Engine), c.Image, c.User, c.Memory, c.CPUs)
	if c.Runtime != "" {
		s += ", runtime " + c.Runtime
	}
	return s
}

// Mount is a bind mount. The path is the same on the host and in the
// container.
type Mount struct {
	Path     string
	Target   string // defaults to Path
	Writable bool
}

// Job is one containerised command.
type Job struct {
	Name        string            // container name, unique
	Labels      map[string]string // container labels
	Dir         string            // working directory
	Env         []string          // the command's whole environment
	Mounts      []Mount
	Network     string // "" means none; else a network the factory created (internal)
	Interactive bool   // keep stdin open
	Cmd         []string
}

// RunArgs returns the arguments of "docker run" for the job. It is a pure
// function so that the exact isolation flags can be tested without Docker.
func (c Config) RunArgs(j Job) []string {
	c = c.withDefaults()
	network := j.Network
	if network == "" {
		network = "none"
	}
	args := []string{"run", "--rm", "--name", j.Name,
		"--network", network,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", c.User,
		"--memory", c.Memory, "--memory-swap", c.Memory,
		"--cpus", c.CPUs,
		"--pids-limit", strconv.Itoa(c.PIDs),
		"--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=" + c.TmpSize,
		"--init",
		"--pull", "missing", "--quiet",
	}
	if c.Podman() && os.Getuid() != 0 {
		args = append(args, "--userns", "keep-id") // rootless: map the host user, so mounted files keep their owner
	}
	if c.Runtime != "" {
		args = append(args, "--runtime", c.Runtime)
	}
	if j.Interactive {
		args = append(args, "--interactive")
	}
	keys := make([]string, 0, len(j.Labels))
	for k := range j.Labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+j.Labels[k])
	}
	if j.Dir != "" {
		args = append(args, "--workdir", j.Dir)
	}
	for _, m := range j.Mounts {
		args = append(args, "--mount", m.spec())
	}
	for _, e := range j.Env {
		args = append(args, "--env", e)
	}
	args = append(args, c.Image)
	return append(args, j.Cmd...)
}

// Podman reports whether the CLI is podman.
func (c Config) Podman() bool { return strings.Contains(filepath.Base(c.Engine), "podman") }

// spec renders the --mount value. Fields are CSV, so a path containing a
// comma or quote is quoted.
func (m Mount) spec() string {
	target := m.Target
	if target == "" {
		target = m.Path
	}
	f := []string{"type=bind", csvField("source=" + m.Path), csvField("target=" + target)}
	if !m.Writable {
		f = append(f, "readonly")
	}
	return strings.Join(f, ",")
}

func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// CLI returns a container CLI command (docker exec, kill, stats, ...) with
// the configured CLI environment.
func (c Config) CLI(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.withDefaults().Engine, args...)
	cmd.Env = c.EnvCLI
	return cmd
}

// Command returns the command that runs the job. When ctx ends, the
// container is removed (killing everything in it), not just the CLI.
func (c Config) Command(ctx context.Context, j Job) *exec.Cmd {
	cmd := c.CLI(ctx, c.RunArgs(j)...)
	cmd.Cancel = func() error {
		c.Remove(j.Name)
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// Remove force-removes a container, ignoring errors (it may be gone).
func (c Config) Remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.CLI(ctx, "rm", "-f", name).Run()
}

// Check verifies that the container engine answers.
func (c Config) Check(ctx context.Context) (string, error) {
	if !c.Contained() {
		return "", nil
	}
	out, err := c.CLI(ctx, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s does not answer (%s): start the container engine or set FLIESSBAND_SANDBOX=none",
			c.withDefaults().Engine, strings.TrimSpace(lastLine(string(out))))
	}
	return strings.TrimSpace(string(out)), nil
}

// Arch returns the CPU architecture of the engine's Linux host, for
// building binaries that run in the container.
func (c Config) Arch(ctx context.Context) string {
	for _, f := range []string{"{{.Server.Arch}}", "{{.Server.OsArch}}"} {
		out, err := c.CLI(ctx, "version", "--format", f).Output()
		if s := strings.TrimSpace(string(out)); err == nil && s != "" && !strings.Contains(s, "<no value>") {
			if i := strings.LastIndexByte(s, '/'); i >= 0 {
				s = s[i+1:]
			}
			return s
		}
	}
	return runtime.GOARCH
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// ParseMemUsage reads the first figure of "docker stats" MemUsage output,
// such as "12.5MiB / 4GiB", in bytes.
func ParseMemUsage(s string) (int64, bool) {
	s = strings.TrimSpace(strings.SplitN(s, "/", 2)[0])
	units := []struct {
		suffix string
		mult   float64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1},
	}
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil {
				return 0, false
			}
			return int64(v * u.mult), true
		}
	}
	return 0, false
}
