package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A worker runs Tart only through the Grove Tart shim, which a login shell's PATH never has. Doctor
// must find it there and ask it, not report "not installed" because PATH has no tart.
func TestCheckTartFindsTheGroveTartShim(t *testing.T) {
	t.Setenv("GROVE_CONFIG", "")
	home := t.TempDir()
	noTartOnPath := func(string) (string, error) { return "", errors.New("not found") }
	opts := Options{Home: home, LookPath: noTartOnPath}

	r := NewFakeRunner()
	got := checkTart(context.Background(), r, opts)
	if got[0].Name != "tart" || got[0].OK || got[0].Detail != "not installed" {
		t.Fatalf("no shim and nothing on PATH: got %+v, want tart not installed", got[0])
	}

	shim := groveTartShimPath(opts)
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r = NewFakeRunner()
	r.Script(shim+" list --format json", FakeResult{Stdout: "[]"})
	got = checkTart(context.Background(), r, opts)
	if !got[0].OK {
		t.Fatalf("shim installed: got %+v, want tart installed", got[0])
	}
	if !r.CalledWith(shim + " list --format json") {
		t.Fatalf("doctor did not ask the shim by its absolute path: calls %v", r.Calls)
	}
	if len(got) < 2 || got[1].Name != "tart-vm-slots" || !got[1].OK {
		t.Fatalf("slots: got %+v", got)
	}
}
