//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

func startDetached(executable string, args []string, cwd string, env []string) (int, error) {
	command := exec.Command(executable, args...)
	command.Dir = cwd
	command.Env = env
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	return pid, nil
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
