//go:build windows

package capture

import (
	"os/exec"
	"syscall"
)

func configureMediaCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
