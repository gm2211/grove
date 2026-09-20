//go:build darwin || linux

package install

import (
	"os/exec"
	"testing"
)

func TestConfigureSecretProcessDetachesControllingTerminal(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	configureSecretProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("secret-consuming process must start in a detached session")
	}
}
