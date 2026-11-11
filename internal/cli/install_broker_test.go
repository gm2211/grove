package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/server"
)

func TestControlPlaneBrokerToken(t *testing.T) {
	cp := &config.Config{}
	cp.Server.Listen, cp.Server.URL = "100.101.229.60:6130", "http://100.101.229.60:6130"
	remote := &config.Config{}
	remote.Server.URL = "http://100.101.229.60:6130"

	type call struct {
		name string
	}
	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		stored    bool
		issueErr  error
		want      string
		wantCalls int
	}{
		{name: "control plane, nothing stored: issues a broker-only credential", cfg: cp, want: "gbt_new", wantCalls: 1},
		{name: "control plane, already stored: keeps it", cfg: cp, stored: true},
		{name: "not the control plane: join approval brings it", cfg: remote},
		{name: "no config", cfg: nil},
		{name: "server down: warns, installs without", cfg: cp, issueErr: errors.New("connection refused"), wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := filepath.Join(t.TempDir(), "broker")
			if tc.stored {
				if err := os.WriteFile(credential, []byte("old\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var calls []call
			issue := func(_ context.Context, name string) (string, string, error) {
				calls = append(calls, call{name})
				if tc.issueErr != nil {
					return "", "", tc.issueErr
				}
				return "gbt_new", "dev_1", nil
			}
			var warn bytes.Buffer
			got := controlPlaneBrokerToken(context.Background(), tc.cfg, credential, "Studio.local", issue, &warn)
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
			if len(calls) != tc.wantCalls {
				t.Fatalf("issue calls = %v, want %d", calls, tc.wantCalls)
			}
			for _, c := range calls {
				if c.name != "Studio.local" {
					t.Errorf("joined as %+v, want the worker's name Studio.local", c)
				}
			}
			if strings.Contains(warn.String(), "gbt_new") {
				t.Errorf("the token was printed: %q", warn.String())
			}
			if tc.issueErr != nil && !strings.Contains(warn.String(), "warning") {
				t.Errorf("no warning on a failed issue: %q", warn.String())
			}
		})
	}
}

// The control plane's own worker gets its broker credential through a broker-only join it
// approves itself, never through POST /access/devices, and the credential is bound to the
// address this host reaches its own `grove serve` from.
func TestSelfJoinBrokerAgainstTheRealServer(t *testing.T) {
	access, err := server.NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	api := server.New(nil, nil, nil, nil, server.Options{Token: "operator-secret", AccessStore: access})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		api.ServeHTTP(w, r)
	}))
	defer ts.Close()

	token, deviceID, err := selfJoinBroker(ts.URL, "operator-secret")(context.Background(), "Studio.local")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := access.Authenticate(token)
	if !ok || p.ID != deviceID || p.Name != "Studio.local" || len(p.Scopes) != 1 || p.Scopes[0] != server.ScopeBroker {
		t.Fatalf("credential = %+v (ok %v), want broker only, named after the worker", p, ok)
	}
	if b, _ := access.Binding(deviceID); b.IP != "127.0.0.1" {
		t.Fatalf("binding = %+v, want the loopback address the join came from", b)
	}
	if n := len(access.List()); n != 1 {
		t.Fatalf("%d credentials issued, want only the broker one (no dispatch credential, no Orchard account)", n)
	}
	for _, path := range paths {
		if strings.Contains(path, "/access/devices") {
			t.Fatalf("the install went through %s", path)
		}
	}

	// A second run (the credential file was lost) replaces the first.
	token2, _, err := selfJoinBroker(ts.URL, "operator-secret")(context.Background(), "Studio.local")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := access.Authenticate(token); ok {
		t.Error("the earlier broker credential should be revoked")
	}
	if _, ok := access.Authenticate(token2); !ok {
		t.Error("the new broker credential must be live")
	}

	if _, _, err := selfJoinBroker(ts.URL, "wrong")(context.Background(), "Studio.local"); err == nil {
		t.Error("a wrong operator token approved a join")
	}
}

// An older `grove serve` ignores brokerOnly and would run a full join: never approve that.
func TestSelfJoinBrokerRefusesAnOlderServer(t *testing.T) {
	approved := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/join/requests":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"i","secret":"s","code":"ABCDEFGH"}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/join/approve/"):
			approved = true
		}
	}))
	defer ts.Close()
	if _, _, err := selfJoinBroker(ts.URL, "operator-secret")(context.Background(), "Studio.local"); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("err = %v, want a refusal that says to restart grove serve", err)
	}
	if approved {
		t.Fatal("the full join was approved")
	}
}

func TestAccessIssueRefusesBrokerScope(t *testing.T) {
	cmd := newAccessIssueCmd()
	cmd.SetArgs([]string{"--name", "mac-1", "--scope", "broker"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if !errors.Is(err, server.ErrBrokerScopeNotIssuable) {
		t.Fatalf("err = %v, want ErrBrokerScopeNotIssuable before any request is made", err)
	}
}
