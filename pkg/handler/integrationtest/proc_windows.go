//go:build windows

package integrationtest

import (
	"os/exec"
	"syscall"
)

func procAttr() *syscall.SysProcAttr {
	return nil
}

func stopProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}
