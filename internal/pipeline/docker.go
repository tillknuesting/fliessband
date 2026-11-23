package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dockerInfo says whether Docker can be used and how to reach it.
type dockerInfo struct {
	ok      bool
	reason  string
	host    string // DOCKER_HOST, resolved from the current docker context
	config  string // isolated client config: no credential helpers, no registry logins
	version string
}

// docker detects Docker, at most once a minute. The factory uses its own
// client configuration so that it never touches the user's registry
// credentials; it only pulls public images.
func (m *Manager) docker() dockerInfo {
	m.dockerMu.Lock()
	defer m.dockerMu.Unlock()
	if !m.dockerSeen.IsZero() && time.Since(m.dockerSeen) < time.Minute {
		return m.dockerInfo
	}
	m.dockerSeen = time.Now()
	info := dockerInfo{}
	defer func() { m.dockerInfo = info }()
	path, err := exec.LookPath("docker")
	if err != nil {
		info.reason = "the docker CLI is not installed"
		return info
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	info.host = os.Getenv("DOCKER_HOST")
	if info.host == "" {
		if out, err := exec.CommandContext(ctx, path, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output(); err == nil {
			info.host = strings.TrimSpace(string(out))
		}
	}
	info.config = filepath.Join(m.dataDir, "docker")
	if err := os.MkdirAll(info.config, 0o700); err != nil {
		info.reason = err.Error()
		return info
	}
	if err := os.WriteFile(filepath.Join(info.config, "config.json"), []byte("{}\n"), 0o600); err != nil {
		info.reason = err.Error()
		return info
	}
	out, err := m.dockerCmd(ctx, info, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		info.reason = "Docker is installed but its daemon is not reachable (start Docker Desktop, Colima or similar)"
		return info
	}
	info.ok, info.version = true, strings.TrimSpace(string(out))
	return info
}

func (m *Manager) dockerCmd(ctx context.Context, d dockerInfo, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "DOCKER_CONFIG=" + d.config}
	if d.host != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+d.host)
	}
	return cmd
}

// postgresDB is a throwaway PostgreSQL container.
type postgresDB struct {
	m         *Manager
	d         dockerInfo
	container string
	url       string
	once      sync.Once
}

// startPostgres starts PostgreSQL on a random loopback port with its data
// in memory, and waits until it accepts TCP connections.
func (m *Manager) startPostgres(ctx context.Context, runID string, log func(string)) (*postgresDB, error) {
	d := m.docker()
	db := &postgresDB{m: m, d: d, container: "fliessband-pg-" + runID}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := m.dockerCmd(ctx, d, "run", "-d", "--rm", "--name", db.container,
		"-e", "POSTGRES_PASSWORD=fliessband", "-p", "127.0.0.1::5432", "--tmpfs", "/var/lib/postgresql/data", postgresImage).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %s", postgresImage, lastLine(string(out)))
	}
	ok := false
	defer func() {
		if !ok {
			db.stop()
		}
	}()
	out, err = m.dockerCmd(ctx, d, "port", db.container, "5432/tcp").Output()
	if err != nil {
		return nil, fmt.Errorf("docker port: %w", err)
	}
	hostPort := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	_, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, fmt.Errorf("docker port printed %q", hostPort)
	}
	// pg_isready over TCP: the image's init phase only listens on a socket.
	for m.dockerCmd(ctx, d, "exec", db.container, "pg_isready", "-q", "-h", "127.0.0.1", "-U", "postgres").Run() != nil {
		select {
		case <-ctx.Done():
			return nil, errors.New("PostgreSQL did not become ready within 3 minutes")
		case <-time.After(300 * time.Millisecond):
		}
	}
	db.url = fmt.Sprintf("postgres://postgres:fliessband@127.0.0.1:%s/postgres?sslmode=disable", port)
	log(fmt.Sprintf("container %s listening on 127.0.0.1:%s", db.container, port))
	ok = true
	return db, nil
}

// stop removes the container.
func (db *postgresDB) stop() {
	db.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = db.m.dockerCmd(ctx, db.d, "rm", "-f", db.container).Run()
	})
}

// cachedTools returns tool versions, refreshed every ten minutes.
func (m *Manager) cachedTools() []Tool {
	m.toolsMu.Lock()
	defer m.toolsMu.Unlock()
	if m.tools == nil || time.Since(m.toolsSeen) > 10*time.Minute {
		m.tools, m.toolsSeen = m.Tools(), time.Now()
	}
	return m.tools
}
