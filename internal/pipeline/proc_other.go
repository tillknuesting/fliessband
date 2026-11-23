//go:build !unix

package pipeline

import (
	"os/exec"
	"time"
)

func isolate(cmd *exec.Cmd) { cmd.WaitDelay = 5 * time.Second }

func processAlive(pid int) bool { return pid > 0 }
