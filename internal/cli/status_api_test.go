package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/server"
)

// A laptop set up for "Snoozing your own Mac" has only server.url and a scoped token (issue #133).
func tokenOnlyConfig(t *testing.T, serverURL string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  url: "+serverURL+"\n  token: laptop-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROVE_CONFIG", cfgPath)
}

func fakeFleetServer(t *testing.T, status int, resp server.FleetResponse) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer laptop-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/v1/fleet" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(resp)
		} else {
			_, _ = w.Write([]byte(`{"error":"orchard is down"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sampleFleet(now time.Time) server.FleetResponse {
	until := now.Add(time.Hour)
	return server.FleetResponse{
		Workers: []server.FleetEntry{
			{Name: "g-macbook-m5.local", Arch: "arm64", Online: true, Cordoned: true, SnoozedUntil: &until,
				Raw: orchard.Worker{Name: "g-macbook-m5.local", Runtime: "tart", LastSeen: now.Add(-3 * time.Second)}},
			{Name: "studio", Arch: "arm64", Online: true, Raw: orchard.Worker{Name: "studio", Runtime: "tart"}},
		},
		VMs: []server.FleetEntry{
			{Name: "linux-studio-0", Status: "running", Raw: orchard.VM{Name: "linux-studio-0", Worker: "studio", Status: "running"}},
			{Name: "macos-g-macbook-m5.local-0", Status: "failed", Raw: orchard.VM{Name: "macos-g-macbook-m5.local-0", Worker: "g-macbook-m5.local", Status: "failed",
				StatusMessage: `Error Domain=VZErrorDomain Code=-9 "The virtual machine encountered a security error." Failed to get current host key`}},
		},
		Nodes: []server.FleetEntry{{Name: "n1", Status: "ready"}, {Name: "n2", Status: "down"}},
	}
}

func TestWorkerLs_TokenOnlyLaptopUsesTheAPI(t *testing.T) {
	srv := fakeFleetServer(t, http.StatusOK, sampleFleet(time.Now()))
	tokenOnlyConfig(t, srv.URL)

	var out, errOut bytes.Buffer
	workerLsCmd.SetOut(&out)
	workerLsCmd.SetErr(&errOut)
	workerLsCmd.SetContext(context.Background())
	t.Cleanup(func() { workerLsCmd.SetOut(nil); workerLsCmd.SetErr(nil) })
	if err := workerLsCmd.RunE(workerLsCmd, nil); err != nil {
		t.Fatalf("worker ls: %v", err)
	}
	got := out.String()
	for _, want := range []string{"g-macbook-m5.local", "tart", "until ", "studio"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got+errOut.String(), "orchard.url") {
		t.Errorf("worker ls fell back to Orchard:\n%s%s", got, errOut.String())
	}
}

func TestWorkerLs_TokenOnlyLaptopReportsTheAPIError(t *testing.T) {
	srv := fakeFleetServer(t, http.StatusBadGateway, server.FleetResponse{})
	tokenOnlyConfig(t, srv.URL)

	workerLsCmd.SetOut(&bytes.Buffer{})
	workerLsCmd.SetErr(&bytes.Buffer{})
	workerLsCmd.SetContext(context.Background())
	t.Cleanup(func() { workerLsCmd.SetOut(nil); workerLsCmd.SetErr(nil) })
	err := workerLsCmd.RunE(workerLsCmd, nil)
	if err == nil || strings.Contains(err.Error(), "orchard.url") || !strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("err = %v, want the API's error, not Orchard's missing URL", err)
	}
}

func TestStatus_TokenOnlyLaptopUsesTheAPI(t *testing.T) {
	srv := fakeFleetServer(t, http.StatusOK, sampleFleet(time.Now()))
	tokenOnlyConfig(t, srv.URL)

	var out bytes.Buffer
	statusCmd.SetOut(&out)
	statusCmd.SetContext(context.Background())
	t.Cleanup(func() { statusCmd.SetOut(nil) })
	if err := statusCmd.RunE(statusCmd, nil); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"server", "OK",
		"workers: 2 (2 online)",
		"vms: 2",
		"nomad nodes: 2 (down 1, ready 1)",
		"snoozed: g-macbook-m5.local until ",
		"needs login: the worker user on g-macbook-m5.local has no login session",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "orchard") || strings.Contains(got, "FAIL") {
		t.Errorf("status still pinged Orchard/Nomad:\n%s", got)
	}
}

func TestStatus_TokenOnlyLaptopFailsOnAPIError(t *testing.T) {
	srv := fakeFleetServer(t, http.StatusBadGateway, server.FleetResponse{})
	tokenOnlyConfig(t, srv.URL)

	var out bytes.Buffer
	statusCmd.SetOut(&out)
	statusCmd.SetContext(context.Background())
	t.Cleanup(func() { statusCmd.SetOut(nil) })
	if err := statusCmd.RunE(statusCmd, nil); err == nil || !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("err = %v, out = %s; want the server check to fail", err, out.String())
	}
}
