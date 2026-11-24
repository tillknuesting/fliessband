//go:build unix

package deploy

import "syscall"

// alive reports whether a process with the pid exists.
func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
