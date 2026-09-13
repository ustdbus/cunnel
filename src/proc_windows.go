//go:build windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
)

func notifyShutdown(c chan os.Signal) {
	signal.Notify(c, os.Interrupt)
}

func setProcSysAttr(cmd *exec.Cmd) {
	// Windows 不需要且不支持 POSIX Setpgid
}

func isProcAlive(p *proc) bool {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	return p.cmd.ProcessState == nil
}

func killProcessGroup(p *proc) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
}
