//go:build unix

package pipeline

import (
	"os/exec"
	"syscall"
	"time"
)

// isolate runs cmd in its own process group and kills the whole group when
// the command's context ends, so test binaries and servers that a tool
// started do not outlive a timeout or a canceled run.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
}

// processAlive reports whether a process with the pid exists.
func processAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
