//go:build linux

package ffmpeg

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the child in its own process group and wires
// cancellation to kill that whole group, so helper processes a tool might
// spawn cannot outlive the request.
func isolateProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
