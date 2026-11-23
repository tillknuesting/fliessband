package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A server in a container without a network cannot be reached over TCP
// from the host. The bridge is a tiny static program, mounted read-only into
// the container, that connects to the server on the container's loopback
// and relays the connection over its stdin and stdout; the factory starts
// it with "docker exec -i" for every client connection. The server keeps no
// network at all.

// BridgePath is where the bridge is mounted in the container.
const BridgePath = "/fliessband/bridge"

// bridgeSource is the bridge program. It writes one byte once connected, so
// the dialer can tell a refused connection from an open one.
const bridgeSource = `package main

import (
	"io"
	"net"
	"os"
)

func main() {
	c, err := net.Dial("tcp", os.Args[1])
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	if _, err := os.Stdout.Write([]byte{'+'}); err != nil {
		os.Exit(1)
	}
	go func() {
		_, _ = io.Copy(c, os.Stdin)
		_ = c.(*net.TCPConn).CloseWrite()
	}()
	_, _ = io.Copy(os.Stdout, c)
}
`

var bridgeMu sync.Mutex

// BuildBridge compiles the bridge for linux/arch into dir with the host's go
// command and returns the binary's path. It is built once per source and
// architecture; it uses only the standard library, so it builds offline.
func BuildBridge(ctx context.Context, dir, arch string, env []string) (string, error) {
	return buildBridge(ctx, dir, "linux", arch, env)
}

func buildBridge(ctx context.Context, dir, goos, arch string, env []string) (string, error) {
	bridgeMu.Lock()
	defer bridgeMu.Unlock()
	src := filepath.Join(dir, "bridge-src")
	bin := filepath.Join(dir, "bridge-"+goos+"-"+arch)
	if err := os.MkdirAll(src, 0o750); err != nil {
		return "", err
	}
	main := filepath.Join(src, "main.go")
	old, _ := os.ReadFile(main)
	if _, err := os.Stat(bin); err == nil && bytes.Equal(old, []byte(bridgeSource)) {
		return bin, nil
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fliessband/bridge\n\ngo 1.22\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(main, []byte(bridgeSource), 0o600); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", bin, ".")
	cmd.Dir = src
	cmd.Env = append(slices.Clone(env), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the sandbox bridge: %s", strings.TrimSpace(string(out)))
	}
	return bin, nil
}

// ExecArgs returns the arguments of "docker exec" that connect through the
// bridge in the container to addr on its loopback.
func ExecArgs(container, addr string) []string {
	return []string{"exec", "--interactive", container, BridgePath, addr}
}

// DialBridge starts the bridge in the container and returns the connection
// it relays. It fails if the server does not accept the connection.
func (c Config) DialBridge(ctx context.Context, container, addr string) (net.Conn, error) {
	cmd := c.CLI(context.WithoutCancel(ctx), ExecArgs(container, addr)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	conn := &execConn{cmd: cmd, r: stdout, w: stdin}
	ack := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		_, err := io.ReadFull(stdout, b)
		if err == nil && b[0] != '+' {
			err = errors.New("unexpected bridge greeting")
		}
		ack <- err
	}()
	select {
	case err := <-ack:
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("bridge to %s: %s", addr, firstNonEmpty(strings.TrimSpace(stderr.String()), err.Error()))
		}
		return conn, nil
	case <-ctx.Done():
		_ = conn.Close()
		return nil, ctx.Err()
	}
}

// execConn is a net.Conn over a process's stdin and stdout. Deadlines are
// not supported; the gRPC client does not need them.
type execConn struct {
	cmd  *exec.Cmd
	r    io.ReadCloser
	w    io.WriteCloser
	once sync.Once
}

func (c *execConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *execConn) Write(b []byte) (int, error) { return c.w.Write(b) }

func (c *execConn) Close() error {
	c.once.Do(func() {
		_ = c.w.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	})
	return nil
}

func (c *execConn) LocalAddr() net.Addr              { return bridgeAddr("host") }
func (c *execConn) RemoteAddr() net.Addr             { return bridgeAddr("sandbox") }
func (c *execConn) SetDeadline(time.Time) error      { return nil }
func (c *execConn) SetReadDeadline(time.Time) error  { return nil }
func (c *execConn) SetWriteDeadline(time.Time) error { return nil }

type bridgeAddr string

func (a bridgeAddr) Network() string { return "sandbox-bridge" }
func (a bridgeAddr) String() string  { return string(a) }
