//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS: no console, so closing the terminal does not
// stop the refresh.
const detachedProcess = 0x00000008

// detach starts cmd without a console and in its own process group, so it survives
// the shell that asked for completion.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
}
