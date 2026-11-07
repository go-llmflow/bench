//go:build !windows

package chaos

import (
	"os/exec"
	"syscall"
)

// ownGroup puts the gateway in its own process group, so that a signal reaches
// it and whatever it spawned, and never the harness.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// signalGroup sends SIGTERM (graceful) or SIGKILL (a crash) to the group.
func signalGroup(cmd *exec.Cmd, graceful bool) {
	sig := syscall.SIGKILL
	if graceful {
		sig = syscall.SIGTERM
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
