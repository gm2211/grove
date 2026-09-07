package install

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
)

// newHangingListener accepts TCP connections and never responds, so tests can verify doctor's
// HTTP checks give up on their own timeout instead of blocking forever.
func newHangingListener() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without ever writing a response.
			go func(c net.Conn) {
				buf := make([]byte, 1)
				_, _ = c.Read(buf)
			}(conn)
		}
	}()
	return ln, nil
}

func findResult(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no CheckResult named %q in %+v", name, results)
	return CheckResult{}
}

func TestRunDoctor_HTTPChecksAgainstHTTPTestServers(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()

	cfg := &config.Config{
		Orchard: config.Endpoint{URL: healthy.URL, Token: "tok"},
		Nomad:   config.Endpoint{URL: unhealthy.URL},
		Server:  config.ServerConfig{URL: "http://127.0.0.1:1", Token: "tok"}, // nothing listening: connection refused
	}
	opts := Options{Home: t.TempDir(), GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	results := RunDoctor(context.Background(), NewFakeRunner(), opts, cfg)

	if r := findResult(t, results, "orchard-controller"); !r.OK {
		t.Errorf("expected orchard-controller check to pass against a 200 server, got %+v", r)
	}
	if r := findResult(t, results, "nomad"); r.OK {
		t.Errorf("expected nomad check to fail against a 503 server, got %+v", r)
	}
	if r := findResult(t, results, "grove-server"); r.OK {
		t.Errorf("expected grove-server check to fail against an unreachable address, got %+v", r)
	}
}

func TestRunDoctor_NoConfigReportsRemediation(t *testing.T) {
	opts := Options{Home: t.TempDir(), GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}
	results := RunDoctor(context.Background(), NewFakeRunner(), opts, nil)
	r := findResult(t, results, "config")
	if r.OK {
		t.Error("expected the config check to fail when cfg is nil")
	}
	if r.Remediation == "" {
		t.Error("expected a remediation hint when config.yaml is missing")
	}
}

func TestRunDoctor_NeverHangs(t *testing.T) {
	// A server that accepts the connection but never responds must not make doctor hang past its
	// per-check timeout.
	ln, err := newHangingListener()
	if err != nil {
		t.Fatalf("newHangingListener: %v", err)
	}
	defer ln.Close()

	cfg := &config.Config{Server: config.ServerConfig{URL: "http://" + ln.Addr().String()}}
	opts := Options{Home: t.TempDir(), GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	done := make(chan struct{})
	go func() {
		RunDoctor(context.Background(), NewFakeRunner(), opts, cfg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("RunDoctor did not return within 15s against a hanging server")
	}
}

// fakeNomadNode is the minimal shape checkMacOSCPUFingerprint cares about, from both the
// /v1/nodes list-stub and /v1/node/<id> full-node responses (internal/nomad.Client.ListNodes
// calls both per node, plus /v1/node/<id>/allocations).
type fakeNomadNode struct {
	ID        string
	Name      string
	NodeClass string
	cpuMHz    int64
}

// newFakeNomadServer serves just enough of the Nomad HTTP API (/v1/nodes, /v1/node/<id>,
// /v1/node/<id>/allocations) for internal/nomad.Client.ListNodes to work against it, so
// checkMacOSCPUFingerprint can be tested without a real Nomad cluster.
func newFakeNomadServer(t *testing.T, nodes []fakeNomadNode) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		stubs := make([]map[string]any, 0, len(nodes))
		for _, n := range nodes {
			stubs = append(stubs, map[string]any{"ID": n.ID, "Name": n.Name, "NodeClass": n.NodeClass})
		}
		_ = json.NewEncoder(w).Encode(stubs)
	})
	for _, n := range nodes {
		n := n
		mux.HandleFunc("/v1/node/"+n.ID, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ID":        n.ID,
				"Name":      n.Name,
				"NodeClass": n.NodeClass,
				"NodeResources": map[string]any{
					"Cpu":    map[string]any{"CpuShares": n.cpuMHz},
					"Memory": map[string]any{"MemoryMB": 8192},
				},
			})
		})
		mux.HandleFunc("/v1/node/"+n.ID+"/allocations", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]any{})
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckMacOSCPUFingerprint_WarnsOnLowCompute(t *testing.T) {
	srv := newFakeNomadServer(t, []fakeNomadNode{
		{ID: "n1", Name: "macos-mac1-0", NodeClass: "macos", cpuMHz: 24}, // the real observed defect
		{ID: "n2", Name: "linux-worker1-0", NodeClass: "linux", cpuMHz: 24},
	})
	cfg := &config.Config{Nomad: config.Endpoint{URL: srv.URL}}

	r := checkMacOSCPUFingerprint(context.Background(), cfg)
	if r.OK {
		t.Fatalf("expected the check to fail on a 24 MHz macOS fingerprint, got %+v", r)
	}
	if !strings.Contains(r.Detail, "macos-mac1-0") {
		t.Errorf("detail = %q, want it to name the offending node", r.Detail)
	}
	if strings.Contains(r.Detail, "linux-worker1-0") {
		t.Errorf("detail = %q, should not flag a non-macos node even with a low fingerprint", r.Detail)
	}
}

func TestCheckMacOSCPUFingerprint_OKWhenHealthy(t *testing.T) {
	srv := newFakeNomadServer(t, []fakeNomadNode{
		{ID: "n1", Name: "macos-mac1-0", NodeClass: "macos", cpuMHz: 36000},
	})
	cfg := &config.Config{Nomad: config.Endpoint{URL: srv.URL}}

	r := checkMacOSCPUFingerprint(context.Background(), cfg)
	if !r.OK {
		t.Errorf("expected OK for a healthy 36000 MHz fingerprint, got %+v", r)
	}
}

func TestCheckMacOSCPUFingerprint_SkippedWithoutNomadConfig(t *testing.T) {
	r := checkMacOSCPUFingerprint(context.Background(), &config.Config{})
	if !r.OK {
		t.Errorf("expected the check to skip (OK) when nomad isn't configured, got %+v", r)
	}
}

func TestRunDoctor_IncludesMacOSCPUFingerprintCheck(t *testing.T) {
	srv := newFakeNomadServer(t, []fakeNomadNode{
		{ID: "n1", Name: "macos-mac1-0", NodeClass: "macos", cpuMHz: 24},
	})
	cfg := &config.Config{Nomad: config.Endpoint{URL: srv.URL}}
	opts := Options{Home: t.TempDir(), GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	results := RunDoctor(context.Background(), NewFakeRunner(), opts, cfg)
	r := findResult(t, results, "macos-cpu-fingerprint")
	if r.OK {
		t.Errorf("expected macos-cpu-fingerprint to fail against the fake low-compute node, got %+v", r)
	}
}

// TestRunDoctor_ReportsUntrustedRequiredTapWithRemediation covers the doctor half of the brew-trust
// fix: a tap that's tapped on this machine but not trusted (e.g. after a Homebrew upgrade adds the
// trust gate under an already-tapped openai/tools) must show up as a failing check with the exact
// `brew trust <tap>` remediation command, matching the message Homebrew itself prints when refusing
// to load a formula from an untrusted tap.
func TestRunDoctor_ReportsUntrustedRequiredTapWithRemediation(t *testing.T) {
	r := NewFakeRunner()
	scriptTapInfo(r, "openai/tools", true, false)
	scriptTapInfo(r, "cirruslabs/cli", false, false)
	scriptTapInfo(r, "hashicorp/tap", false, false)
	scriptTapInfo(r, "minio/stable", false, false)
	opts := Options{Home: t.TempDir(), GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	results := RunDoctor(context.Background(), r, opts, nil)
	got := findResult(t, results, "brew-trust:openai/tools")
	if got.OK {
		t.Error("expected the untrusted openai/tools tap to fail the check")
	}
	if got.Remediation != "brew trust openai/tools" {
		t.Errorf("Remediation = %q, want the exact `brew trust openai/tools` command", got.Remediation)
	}

	for _, r := range results {
		if r.Name == "brew-trust:cirruslabs/cli" || r.Name == "brew-trust:hashicorp/tap" || r.Name == "brew-trust:minio/stable" {
			t.Errorf("a tap that was never tapped on this machine should not be reported, got %+v", r)
		}
	}
}

// TestRunDoctor_SkipsTapTrustChecksOnNonDarwin makes sure doctor doesn't try to shell out to `brew`
// (which doesn't exist) when run on Linux.
func TestRunDoctor_SkipsTapTrustChecksOnNonDarwin(t *testing.T) {
	r := NewFakeRunner()
	opts := Options{Home: t.TempDir(), GOOS: "linux", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	results := RunDoctor(context.Background(), r, opts, nil)
	for _, res := range results {
		if len(res.Name) >= len("brew-trust:") && res.Name[:len("brew-trust:")] == "brew-trust:" {
			t.Errorf("did not expect any brew-trust check on non-darwin, got %+v", res)
		}
	}
	if r.CalledWith("brew tap-info --json=v1 openai/tools") {
		t.Error("no brew command should run on non-darwin")
	}
}

// writeFleetYAML writes a minimal valid fleet.yaml with one pool pulling image, returning its path.
func writeFleetYAML(t *testing.T, dir, image string) string {
	t.Helper()
	path := filepath.Join(dir, "fleet.yaml")
	content := "pools:\n  - name: macos\n    image: " + image + "\n    perWorker: 1\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestRunDoctor_WarnsOnUnauthenticatedGHCRImages covers the doctor half of the registry-login
// feature: fleet.yaml pulling a ghcr.io image with no recorded `tart login ghcr.io` on this
// machine must fail the registry-login check with the exact remediation text (make the packages
// public, or re-run `grove install --role worker --registry-token …`).
func TestRunDoctor_WarnsOnUnauthenticatedGHCRImages(t *testing.T) {
	home := t.TempDir()
	fleetPath := writeFleetYAML(t, home, "ghcr.io/gm2211/grove-macos-worker:latest")
	cfg := &config.Config{Fleet: fleetPath}
	opts := Options{Home: home, GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	results := RunDoctor(context.Background(), NewFakeRunner(), opts, cfg)
	r := findResult(t, results, "registry-login")
	if r.OK {
		t.Error("expected registry-login to fail: ghcr.io image with no login marker recorded")
	}
	if !strings.Contains(r.Remediation, "Change visibility") || !strings.Contains(r.Remediation, "--registry-token") {
		t.Errorf("Remediation = %q, want both the public-visibility and --registry-token options", r.Remediation)
	}
}

// TestRunDoctor_RegistryLoginOKWhenMarkerPresent covers the healthy path: once
// registryLoginStep's Apply has written the marker (i.e. `grove install --role worker
// --registry-token …` succeeded), doctor reports registry-login as OK.
func TestRunDoctor_RegistryLoginOKWhenMarkerPresent(t *testing.T) {
	home := t.TempDir()
	fleetPath := writeFleetYAML(t, home, "ghcr.io/gm2211/grove-macos-worker:latest")
	cfg := &config.Config{Fleet: fleetPath}
	opts := Options{Home: home, GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	markerDir := filepath.Join(home, ".config", "grove")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "registry-login.ghcr.io"), []byte("gm2211 ghcr.io\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	results := RunDoctor(context.Background(), NewFakeRunner(), opts, cfg)
	r := findResult(t, results, "registry-login")
	if !r.OK {
		t.Errorf("expected registry-login to pass once the marker is recorded, got %+v", r)
	}
}

// TestRunDoctor_SkipsRegistryLoginCheckForNonGHCRImages makes sure a fleet.yaml that doesn't
// reference ghcr.io at all (e.g. a self-hosted registry, or images built locally) doesn't get a
// registry-login warning — grove only knows ghcr.io to be private-by-default.
func TestRunDoctor_SkipsRegistryLoginCheckForNonGHCRImages(t *testing.T) {
	home := t.TempDir()
	fleetPath := writeFleetYAML(t, home, "my-registry.example.com/grove-macos-worker:latest")
	cfg := &config.Config{Fleet: fleetPath}
	opts := Options{Home: home, GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}

	results := RunDoctor(context.Background(), NewFakeRunner(), opts, cfg)
	for _, r := range results {
		if r.Name == "registry-login" {
			t.Errorf("did not expect a registry-login check for a non-ghcr.io image, got %+v", r)
		}
	}
}

func TestCountRunningMacOSVMs(t *testing.T) {
	n, err := countRunningMacOSVMs(`[
		{"Source":"local","Name":"macos-worker1-0","Running":true,"State":"running"},
		{"Source":"local","Name":"macos-worker1-1","Running":false,"State":"stopped"},
		{"Source":"local","Name":"linux-worker1-0","Running":true,"State":"running"}
	]`)
	if err != nil {
		t.Fatalf("countRunningMacOSVMs: %v", err)
	}
	if n != 1 {
		t.Errorf("countRunningMacOSVMs = %d, want 1", n)
	}
}
