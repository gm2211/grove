package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokerFetchReadsItsCredentialOnceFound(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/broker/entries" {
			http.NotFound(w, r)
			return
		}
		auth = append(auth, r.Header.Get("Authorization"))
		w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	reads := 0
	stored := false
	fetch := brokerFetch(srv.URL, func(context.Context) (string, error) {
		reads++
		if !stored {
			return "", errors.New("no broker credential in Keychain")
		}
		return "broker-token", nil
	}, nil)
	ctx := context.Background()
	if _, err := fetch(ctx); err == nil {
		t.Fatal("fetch without a credential succeeded")
	}
	if len(auth) != 0 {
		t.Fatal("called the control plane without a credential")
	}
	stored = true
	for i := 0; i < 3; i++ {
		if _, err := fetch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 {
		t.Errorf("Keychain read %d times, want once more after the credential appeared", reads)
	}
	for _, a := range auth {
		if a != "Bearer broker-token" {
			t.Errorf("Authorization = %q", a)
		}
	}
}

type brokerTartRunner struct{ ran []string }

func (r *brokerTartRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	r.ran = append(r.ran, name+" "+strings.Join(args, " "))
	const vm = "orchard-vm-a-0b1e5a3c-1f0e-4c56-9d2a-6f3c2b1a0d9e-0"
	if len(args) > 0 && args[0] == "list" {
		return `[{"Name":"` + vm + `","Source":"local","Running":true}]`, "", nil
	}
	return "192.168.64.5\n", "", nil
}

// The broker asks the worker's own Tart, by absolute path: the Grove Tart shim the worker daemon's
// wrapper puts first on PATH, never a bare `tart` (Homebrew's, or whatever launchd's PATH finds).
func TestBrokerSourcesRunOnlyTheGroveTart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROVE_CONFIG", "")
	shim := filepath.Join(home, ".config", "grove", "tart-bin", "tart")

	r := &brokerTartRunner{}
	src := brokerSources(r)
	if src.Bin != shim {
		t.Fatalf("Bin = %q, want the Grove Tart shim %q", src.Bin, shim)
	}
	addrs, failed := src.Sources(context.Background(), []string{"vm-a"})
	if addrs["vm-a"] != netip.MustParseAddr("192.168.64.5") || len(failed) != 0 {
		t.Fatalf("addrs %v, failed %v", addrs, failed)
	}
	if len(r.ran) != 2 {
		t.Fatalf("ran %q, want a list and an ip", r.ran)
	}
	for _, c := range r.ran {
		if !strings.HasPrefix(c, shim+" ") {
			t.Errorf("ran %q, want only %s", c, shim)
		}
	}
}
