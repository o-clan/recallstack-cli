//go:build windows

package hooks

import (
	"os/exec"
	"syscall"
)

func startDetached(executable string, args []string, cwd string, env []string) (int, error) {
	command := exec.Command(executable, args...)
	command.Dir = cwd
	command.Env = env
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200}
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	return pid, nil
}
