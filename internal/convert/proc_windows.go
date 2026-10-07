package convert

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a console converter from flashing a console window when
// it is started by the windowless desktop app.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
