//go:build !unix

package deploy

// alive cannot check other processes here, so it trusts the record.
func alive(pid int) bool { return pid > 0 }
