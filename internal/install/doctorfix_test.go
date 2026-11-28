package install

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// `sudo grove doctor --fix` never runs, as root, a grove the worker user could have written.
func TestFixAsRootRefusesAWorkerWritableGrove(t *testing.T) {
	uid, r := helperEnv(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if me.Uid == "0" {
		t.Skip("needs a non-root worker user")
	}
	cfg, _ := json.Marshal(RootHelperConfig{Version: 1, User: me.Username, UID: uid})
	_ = os.WriteFile(rootHelperConfigPath(), cfg, 0o644)
	exe := filepath.Join(t.TempDir(), "grove")
	_ = os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	err = FixAsRoot(context.Background(), r, exe, "v", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "root won't run it") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(rootHelperBin()); err == nil {
		t.Fatal("installed a worker-writable binary as root's")
	}
}

func TestFindWorkerUserFromHelperConfig(t *testing.T) {
	uid, _ := helperEnv(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if me.Uid == "0" {
		t.Skip("the helper config refuses root")
	}
	cfg, _ := json.Marshal(RootHelperConfig{Version: 1, User: me.Username, UID: uid})
	_ = os.WriteFile(rootHelperConfigPath(), cfg, 0o644)
	u, err := FindWorkerUser("")
	if err != nil || u.Username != me.Username {
		t.Fatalf("FindWorkerUser = %v, %v", u, err)
	}
}
