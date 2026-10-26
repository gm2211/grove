//go:build darwin || linux

package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortSocketPath keeps the path under the Unix socket length limit (104 bytes on macOS).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "api.sock")
}

func unixClient(path string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
}

func TestListenLocal_OnlyAllowedUsersConnect(t *testing.T) {
	path := shortSocketPath(t)
	l, err := ListenLocal(path, []uint32{uint32(os.Getuid())}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode = %v, %v; want 0666 (the peer check is the gate)", fi.Mode(), err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !viaLocalSocket(r.Context()) {
			t.Error("request not marked as from the local socket")
		}
		io.WriteString(w, "hello")
	}), ConnContext: LocalConnContext}
	go srv.Serve(l)
	defer srv.Close()

	resp, err := unixClient(path).Get("http://grove/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}

	// Another user's connection is closed unread.
	other, err := ListenLocal(shortSocketPath(t), []uint32{uint32(os.Getuid()) + 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a user not allowed to connect got a request through")
	}), ConnContext: LocalConnContext}
	go srv2.Serve(other)
	defer srv2.Close()
	if _, err := unixClient(other.Addr().String()).Get("http://grove/healthz"); err == nil {
		t.Fatal("a user not allowed to connect got a response")
	}
}

func TestListenLocal_ReplacesOnlyAStaleSocket(t *testing.T) {
	path := shortSocketPath(t)
	first, err := ListenLocal(path, []uint32{uint32(os.Getuid())}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first.(*localListener).Listener.(*net.UnixListener).SetUnlinkOnClose(false)
	first.Close()
	second, err := ListenLocal(path, []uint32{uint32(os.Getuid())}, nil)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	second.Close()

	file := filepath.Join(filepath.Dir(path), "not-a-socket")
	if err := os.WriteFile(file, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenLocal(file, []uint32{uint32(os.Getuid())}, nil); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("ListenLocal over a regular file: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != "keep me" {
		t.Fatal("a regular file was replaced")
	}
	if _, err := ListenLocal(path, nil, nil); err == nil {
		t.Fatal("a socket nobody may connect to was opened")
	}
}

func TestLocalSocketNeedsAToken(t *testing.T) {
	srv, _ := tailscaleServer(t, nil)
	local := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		req.Host = "grove"
		req.RemoteAddr = argosPeer // even an address Tailscale would accept counts for nothing here
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req = req.WithContext(LocalConnContext(req.Context(), nil))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := local(""); code != http.StatusUnauthorized {
		t.Fatalf("local socket without a token: %d, want 401", code)
	}
	if code := local("operator"); code != http.StatusOK {
		t.Fatalf("local socket with a token: %d, want 200", code)
	}
}
