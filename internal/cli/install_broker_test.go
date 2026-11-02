package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/server"
)

func TestControlPlaneBrokerToken(t *testing.T) {
	cp := &config.Config{}
	cp.Server.Listen, cp.Server.URL = "100.101.229.60:6130", "http://100.101.229.60:6130"
	remote := &config.Config{}
	remote.Server.URL = "http://100.101.229.60:6130"

	type call struct {
		name   string
		scopes []string
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
			issue := func(_ context.Context, name string, scopes []string, expiresIn time.Duration) (string, server.Principal, error) {
				calls = append(calls, call{name, scopes})
				if expiresIn != 0 {
					t.Errorf("expiresIn = %v, want never (the broker would stop syncing)", expiresIn)
				}
				if tc.issueErr != nil {
					return "", server.Principal{}, tc.issueErr
				}
				return "gbt_new", server.Principal{ID: "dev_1"}, nil
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
				if c.name != "Studio.local" || len(c.scopes) != 1 || c.scopes[0] != server.ScopeBroker {
					t.Errorf("issued %+v, want name Studio.local with only the broker scope", c)
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
