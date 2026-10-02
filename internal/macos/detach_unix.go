//go:build unix

package macos

import (
	"os/exec"
	"syscall"
)

// detach puts the tart process in its own process group so VMs survive an
// agent restart; the agent adopts them again with Attach.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
