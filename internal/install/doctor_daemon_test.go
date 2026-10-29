package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckWorkerDaemon(t *testing.T) {
	ctx := context.Background()
	r := NewFakeRunner()
	opts := testWorkerOptions(t.TempDir())
	d := workerDaemons(opts)[0]

	if c := checkWorkerDaemon(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "does not start at boot") || !strings.Contains(c.Remediation, "sudo install") {
		t.Fatalf("not installed: %+v", c)
	}

	plist, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	_, installed := daemonPaths(opts, d.Label)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte(plist+"<!-- edited -->"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkWorkerDaemon(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "not the one this grove renders") {
		t.Fatalf("edited plist: %+v", c)
	}

	if err := os.WriteFile(installed, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Script("launchctl print system/com.grove.orchard-worker", FakeResult{Err: os.ErrNotExist})
	if c := checkWorkerDaemon(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "not loaded") {
		t.Fatalf("not loaded: %+v", c)
	}

	r.Script("launchctl print system/com.grove.orchard-worker", FakeResult{Stdout: "program = " + d.Program + "\n"})
	if c := checkWorkerDaemon(ctx, r, opts); !c.OK || !strings.Contains(c.Detail, "worker-user") {
		t.Fatalf("healthy: %+v", c)
	}

	// A LaunchAgent left over would start a second worker at the next login.
	if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyAgentPlist(opts, d.Label), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkWorkerDaemon(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "second worker") {
		t.Fatalf("legacy agent remains: %+v", c)
	}

	opts.User = "root"
	if c := checkWorkerDaemon(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "not root") {
		t.Fatalf("root: %+v", c)
	}
}

func TestWorkerInstalled(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	if workerInstalled(opts) {
		t.Fatal("empty Mac")
	}
	for name, path := range map[string]string{
		"wrapper":         workerWrapperPath(opts),
		"LaunchAgent":     legacyAgentPlist(opts, orchardWorkerLabel),
		"LaunchDaemon":    filepath.Join(opts.launchDaemonsDir(), orchardWorkerLabel+".plist"),
		"daemon plist ok": filepath.Join(opts.launchDaemonsDir(), orchardWorkerLabel+".plist"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !workerInstalled(opts) {
			t.Errorf("%s should count as a worker", name)
		}
		os.Remove(path)
	}
}

func TestCheckServiceUnits_SeesLaunchDaemons(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	if c := checkServiceUnits(opts); c.OK {
		t.Fatalf("nothing installed: %+v", c)
	}
	_, installed := daemonPaths(opts, orchardWorkerLabel)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkServiceUnits(opts); !c.OK || !strings.Contains(c.Detail, "com.grove.orchard-worker.plist") {
		t.Fatalf("a worker daemon: %+v", c)
	}
}

func TestRunDoctor_ChecksTheWorkerDaemonOfAnInstalledWorker(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	opts.LookPath = alwaysFailLookPath
	results := RunDoctor(context.Background(), NewFakeRunner(), opts, nil)
	for _, r := range results {
		if r.Name == "worker-daemon" || r.Name == "broker" {
			t.Fatalf("no worker here, but doctor reported %+v", r)
		}
	}
	// Only the wrapper is there (an install interrupted before the admin step).
	if err := os.MkdirAll(filepath.Dir(workerWrapperPath(opts)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workerWrapperPath(opts), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	results = RunDoctor(context.Background(), NewFakeRunner(), opts, nil)
	if c := findResult(t, results, "worker-daemon"); c.OK {
		t.Errorf("an installed wrapper with no daemon must fail: %+v", c)
	}
	findResult(t, results, "broker")
}
