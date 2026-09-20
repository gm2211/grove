//go:build !darwin && !linux

package install

import "os/exec"

func configureSecretProcess(_ *exec.Cmd) {}
