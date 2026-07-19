//go:build !windows

package capture

import "os/exec"

func configureMediaCommand(command *exec.Cmd) {
	_ = command
}
