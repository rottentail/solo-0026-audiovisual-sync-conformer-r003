//go:build !linux

package ffmpeg

import "os/exec"

// isolateProcessGroup keeps the default process handling on platforms where
// the pinned Linux toolchain does not run. The binaries are Linux-only, but the
// package must still compile for tooling on other hosts.
func isolateProcessGroup(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Kill()
	}
}
