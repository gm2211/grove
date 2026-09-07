package install

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
