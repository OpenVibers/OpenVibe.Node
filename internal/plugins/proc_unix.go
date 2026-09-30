//go:build !windows

package plugins

import (
	"os/exec"
	"syscall"
)

// setProcAttr puts the plugin in its own process group so killing it also ends its children (speech synthesis,
// camera helpers).
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
