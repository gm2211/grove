package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/install"
)

func tailnetStatus(t *testing.T) *install.TailscaleStatus {
	t.Helper()
	var st install.TailscaleStatus
	raw := `{"Self":{"DNSName":"me.tailnet.ts.net.","TailscaleIPs":["100.64.0.9"]},"BackendState":"Running",
	"Peer":{"a":{"DNSName":"cp.tailnet.ts.net.","TailscaleIPs":["100.64.0.2","fd7a::2"],"Online":true},
	        "b":{"DNSName":"rogue.tailnet.ts.net.","TailscaleIPs":["100.64.0.1"],"Online":true},
	        "c":{"DNSName":"off.tailnet.ts.net.","TailscaleIPs":["100.64.0.3"],"Online":false}}}`
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

func TestDiscoverGroveReturnsEveryAnsweringPeerSorted(t *testing.T) {
	var probed []string
	probe := func(_ context.Context, base string) bool {
		probed = append(probed, base)
		return base != "http://100.64.0.9:6130"
	}
	got := discoverGrove(context.Background(), tailnetStatus(t), probe)
	if len(got) != 2 || got[0].URL != "http://100.64.0.1:6130" || got[0].Name != "rogue.tailnet.ts.net" ||
		got[1].URL != "http://100.64.0.2:6130" || got[1].Name != "cp.tailnet.ts.net" {
		t.Fatalf("candidates = %+v", got)
	}
	for _, p := range probed {
		if strings.Contains(p, "100.64.0.3") || strings.Contains(p, "fd7a") {
			t.Errorf("offline peers and IPv6 addresses must not be probed: %s", p)
		}
	}
}

func TestChooseGrove(t *testing.T) {
	one := []groveCandidate{{URL: "http://100.64.0.2:6130", Name: "cp.tailnet.ts.net"}}
	two := append([]groveCandidate{{URL: "http://100.64.0.1:6130", Name: "rogue.tailnet.ts.net"}}, one...)
	tests := []struct {
		name        string
		found       []groveCandidate
		input       string
		interactive bool
		yes         bool
		want        string // "" means an error is expected
	}{
		{"none", nil, "", true, true, ""},
		{"single confirmed", one, "y\n", true, false, one[0].URL},
		{"single declined", one, "n\n", true, false, ""},
		{"single empty answer defaults to no", one, "\n", true, false, ""},
		{"single unattended without --yes", one, "", false, false, ""},
		{"single with --yes", one, "", false, true, one[0].URL},
		{"multiple picks chosen", two, "2\n", true, false, two[1].URL},
		{"multiple invalid choice", two, "3\n", true, false, ""},
		{"multiple never auto-picked by --yes", two, "", false, true, ""},
		{"multiple unattended", two, "", false, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := chooseGrove(tc.found, strings.NewReader(tc.input), &out, tc.interactive, tc.yes)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
			// The operator always sees what was discovered, including the peer's name.
			for _, c := range tc.found {
				if !strings.Contains(out.String(), c.URL) || !strings.Contains(out.String(), c.Name) {
					t.Errorf("output does not show %s (%s): %q", c.URL, c.Name, out.String())
				}
			}
		})
	}
}
