//go:build !unix

package macos

import "os/exec"

// detach is a no-op where tart cannot run anyway.
func detach(cmd *exec.Cmd) {}
