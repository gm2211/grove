package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func TestClientPlan_WritesConfigOnly(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := Options{
		Role:      RoleClient,
		ServerURL: "https://grove-cp.tailnet.ts.net:6130",
		Token:     "client-tok",
		Home:      home,
		GOOS:      "darwin",
		LookPath:  lookPathOnly("tailscale"),
		Exists:    alwaysFalseExists,
	}

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(steps) != 2 { // common tailscale check + config write
		t.Fatalf("client plan has %d steps, want 2 (tailscale + config)", len(steps))
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	cfgPath := filepath.Join(home, ".config", "grove", "config.yaml")
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatalf("loading rendered config.yaml: %v", err)
	}
	if cfg.Server.URL != opts.ServerURL {
		t.Errorf("cfg.Server.URL = %q, want %q", cfg.Server.URL, opts.ServerURL)
	}
	if cfg.Server.Token != opts.Token {
		t.Errorf("cfg.Server.Token = %q, want %q", cfg.Server.Token, opts.Token)
	}

	// A client installs no dependencies: the only external command should be the read-only
	// tailscale status check, never anything that installs or mutates.
	for _, c := range r.Calls {
		if c.Name != "/usr/bin/tailscale" {
			t.Errorf("client role shouldn't invoke any external command besides tailscale status, got: %s", c.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents")); !os.IsNotExist(err) {
		t.Errorf("client role should render no launch agents")
	}
}

func TestClientPlan_RequiresServerURL(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	opts := Options{
		Role:     RoleClient,
		Home:     home,
		GOOS:     "darwin",
		LookPath: alwaysFailLookPath,
		Exists:   alwaysFalseExists,
	}
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err == nil {
		t.Error("expected an error when --server is missing for --role client")
	}
}
