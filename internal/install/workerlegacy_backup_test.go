package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func legacyBackupFixture(t *testing.T) Options {
	t.Helper()
	opts := testWorkerOptions(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(legacyWorkerScriptPath(opts)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyWorkerScriptPath(opts), []byte("#!/bin/sh\norchard worker run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyWorkerBootstrapPath(opts), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	return opts
}

func runBackupStep(t *testing.T, opts Options, at time.Time) (satisfiedBefore bool) {
	t.Helper()
	opts.Now = func() time.Time { return at }
	step := backupLegacyWorkerStep(opts, newBackupSession(opts, io.Discard))
	ok, err := step.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		if err := step.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return ok
}

func backupNames(t *testing.T, opts Options) []string {
	t.Helper()
	entries, _ := os.ReadDir(backupsRoot(opts))
	var n []string
	for _, e := range entries {
		n = append(n, e.Name())
	}
	return n
}

func TestBackupLegacyWorker_RepeatedRunCreatesNoNewDir(t *testing.T) {
	opts := legacyBackupFixture(t)
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if runBackupStep(t, opts, t0) {
		t.Fatal("first run reported satisfied")
	}
	for i := 1; i <= 3; i++ {
		if !runBackupStep(t, opts, t0.Add(time.Duration(i)*time.Hour)) {
			t.Errorf("run %d: want [ok], the legacy files are already backed up", i)
		}
	}
	if got := backupNames(t, opts); len(got) != 1 || got[0] != "20261101-100000" {
		t.Errorf("backup dirs = %v, want only the first", got)
	}
	// A changed file is a new backup.
	if err := os.WriteFile(legacyWorkerBootstrapPath(opts), []byte("new-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runBackupStep(t, opts, t0.Add(5*time.Hour)) {
		t.Error("a changed file counts as backed up")
	}
	if got := backupNames(t, opts); len(got) != 2 {
		t.Errorf("backup dirs = %v, want a second for the changed file", got)
	}
}

func TestBackupLegacyWorker_PrunesDuplicatesKeepsOldestAndManual(t *testing.T) {
	opts := legacyBackupFixture(t)
	root := backupsRoot(opts)
	entry := backupEntryName(legacyWorkerScriptPath(opts))
	script, _ := os.ReadFile(legacyWorkerScriptPath(opts))
	token, _ := os.ReadFile(legacyWorkerBootstrapPath(opts))
	write := func(dir string, files map[string][]byte) {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		for n, b := range files {
			if err := os.WriteFile(filepath.Join(root, dir, n), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	all := map[string][]byte{entry: script, backupEntryName(legacyWorkerBootstrapPath(opts)): token}
	write("20261101-104506", all)
	write("20261101-171423", all)
	write("20261101-180337", all)
	write("20261101-182251", map[string][]byte{entry: []byte("different")}) // distinct: kept
	write("manual-20261101-171406", all)
	write("notes", all)
	opts.Now = func() time.Time { return time.Date(2026, 11, 1, 19, 0, 0, 0, time.UTC) }
	step := backupLegacyWorkerStep(opts, newBackupSession(opts, io.Discard))
	if ok, _ := step.Check(context.Background()); ok {
		t.Fatal("duplicate dirs present but the step is satisfied")
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := backupNames(t, opts)
	want := []string{"20261101-104506", "20261101-182251", "manual-20261101-171406", "notes"}
	if len(got) != len(want) {
		t.Fatalf("dirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dirs = %v, want %v", got, want)
		}
	}
	if ok, _ := step.Check(context.Background()); !ok {
		t.Error("step still not satisfied after pruning")
	}
}
