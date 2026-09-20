//go:build darwin || linux

package install

import (
	"os/exec"
	"syscall"
)

func configureSecretProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
