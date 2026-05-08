package proc

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW prevents Windows from allocating a new console window for
// a child process when the parent has no console of its own (as is the case
// for our detached daemon). Without this, every `git`/`tasklist` invocation
// flashes a console window over the user's other windows.
const createNoWindow = 0x08000000

// HideWindow configures cmd so that, on Windows, no console window is
// created for the child process. No-op on other platforms.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
