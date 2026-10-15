//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
