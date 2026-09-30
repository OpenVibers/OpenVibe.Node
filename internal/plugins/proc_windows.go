//go:build windows

package plugins

import "os/exec"

func setProcAttr(cmd *exec.Cmd) {}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
