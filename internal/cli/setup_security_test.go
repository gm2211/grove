package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

func TestSaveJoinedServerKeepsDeviceTokenOutOfConfigAndArgv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("GROVE_CONFIG", path)
	runner := install.NewFakeRunner()
	const fakeSecret = "fake-device-secret-for-test"
	if err := config.Save(path, &config.Config{Server: config.ServerConfig{Token: "existing-operator-token"}}); err != nil {
		t.Fatal(err)
	}
	if err := saveJoinedServer(context.Background(), runner, "http://100.64.0.1:6130", "device-1", fakeSecret); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Token != "existing-operator-token" || cfg.Server.TokenKeychain != "device-1" {
		t.Fatalf("server credential config = %+v", cfg.Server)
	}
	for _, call := range runner.Calls {
		if strings.Contains(call.String(), fakeSecret) {
			t.Fatalf("secret appeared in process arguments: %s", call.String())
		}
	}
	deleteKeychainCommand := "/usr/bin/security delete-generic-password -a device-1 -s " + config.ServerTokenKeychainService
	if !runner.CalledWith(deleteKeychainCommand) {
		t.Fatal("dispatcher credential replacement must delete stale Keychain item before adding")
	}
	var keychainStdin string
	for _, call := range runner.Calls {
		if call.Name == "/usr/bin/expect" {
			keychainStdin = call.Stdin
		}
	}
	if keychainStdin != "device-1\n"+config.ServerTokenKeychainService+"\n"+fakeSecret+"\n" {
		t.Fatal("fake secret was not sent to Keychain through stdin")
	}
}
