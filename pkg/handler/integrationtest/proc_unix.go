//go:build !windows

package integrationtest

import (
	"os/exec"
	"syscall"
)

func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// stopProcess kills the entire process group rooted at cmd. On Unix,
// SIGTERM-then-SIGKILL gives ocifactory a chance to flush logs and
// drain handlers before we forcibly take it down.
func stopProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
}
