//go:build windows

package chaos

import "os/exec"

func ownGroup(*exec.Cmd) {}

// signalGroup: Windows has no SIGTERM for another process; both are a kill.
func signalGroup(cmd *exec.Cmd, _ bool) { _ = cmd.Process.Kill() }
