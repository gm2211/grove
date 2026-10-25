package tsauth

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
)

const taggedWhoIs = `{
  "Node": {"ID": 123, "StableID": "nARGOS", "Name": "argos.tail1234.ts.net.", "Tags": ["tag:argos"]},
  "UserProfile": {"LoginName": "tagged-devices"},
  "CapMap": {"github.com/gm2211/grove": [{"scopes": ["read", "dispatch"]}], "other/cap": [{}]}
}`

const userWhoIs = `{
  "Node": {"StableID": "nLAPTOP", "Name": "laptop.tail1234.ts.net."},
  "UserProfile": {"LoginName": "giulio@example.com"}
}`

func TestParseWhoIs(t *testing.T) {
	id, err := parseWhoIs([]byte(taggedWhoIs))
	if err != nil {
		t.Fatal(err)
	}
	if id.StableID != "nARGOS" || id.Node != "argos.tail1234.ts.net" || id.Login != "" ||
		strings.Join(id.Tags, ",") != "tag:argos" || strings.Join(id.CapScopes, ",") != "read,dispatch" {
		t.Fatalf("tagged = %+v", id)
	}
	id, err = parseWhoIs([]byte(userWhoIs))
	if err != nil {
		t.Fatal(err)
	}
	if id.Login != "giulio@example.com" || len(id.CapScopes) != 0 {
		t.Fatalf("user = %+v", id)
	}
	if _, err := parseWhoIs([]byte(`{"Node": {}}`)); err == nil {
		t.Fatal("accepted an answer naming no node")
	}
}

func TestTailnetAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"100.101.102.103:5555":    true,
		"[fd7a:115c:a1e0::1]:443": true,
		"[::ffff:100.64.0.1]:1":   true,
		"127.0.0.1:6120":          false,
		"192.168.1.10:6120":       false,
		"100.128.0.1:1":           false, // outside 100.64.0.0/10
		"[fd7a:115c:a1e1::1]:1":   false,
		"not-an-address":          false,
	} {
		if _, ok := TailnetAddr(addr); ok != want {
			t.Errorf("TailnetAddr(%q) = %v, want %v", addr, ok, want)
		}
	}
}

type fakeWhoIs struct {
	answers map[string]string
	calls   int
}

func (f *fakeWhoIs) WhoIs(_ context.Context, addr netip.Addr) (Identity, error) {
	f.calls++
	raw, ok := f.answers[addr.String()]
	if !ok {
		return Identity{}, errors.New("no such peer")
	}
	return parseWhoIs([]byte(raw))
}

func TestAuthenticate(t *testing.T) {
	whois := &fakeWhoIs{answers: map[string]string{"100.64.0.1": taggedWhoIs, "100.64.0.2": userWhoIs}}
	a := New(whois, []config.TailscaleGrant{
		{Match: "tag:argos", Scopes: []string{"dispatch:agent"}},
		{Match: "giulio@example.com", Scopes: []string{"operator"}},
	})

	res, ok := a.Authenticate(context.Background(), "100.64.0.1:40000")
	if !ok || res.ID != "tailscale:nARGOS" || strings.Join(res.Scopes, ",") != "dispatch,dispatch:agent,read" ||
		strings.Join(res.Tags, ",") != "tag:argos" {
		t.Fatalf("tagged node: %+v ok=%v", res, ok)
	}
	res, ok = a.Authenticate(context.Background(), "100.64.0.2:40000")
	if !ok || res.ID != "tailscale:nLAPTOP" || strings.Join(res.Scopes, ",") != "operator" ||
		res.Name != "giulio@example.com on laptop.tail1234.ts.net" {
		t.Fatalf("user node: %+v ok=%v", res, ok)
	}
	if _, ok := a.Authenticate(context.Background(), "127.0.0.1:1"); ok {
		t.Fatal("authenticated a loopback peer")
	}
	if _, ok := a.Authenticate(context.Background(), "100.64.0.9:1"); ok {
		t.Fatal("authenticated a peer tailscaled doesn't know")
	}

	// A known peer that nothing grants gets nothing.
	bare := New(&fakeWhoIs{answers: map[string]string{"100.64.0.2": userWhoIs}}, nil)
	if _, ok := bare.Authenticate(context.Background(), "100.64.0.2:1"); ok {
		t.Fatal("authenticated a peer with no grants")
	}
}

func TestLoginGrantIgnoresTaggedNodes(t *testing.T) {
	// A tagged node's whois still carries a UserProfile; a login grant must not match it.
	raw := strings.Replace(taggedWhoIs, `"tagged-devices"`, `"giulio@example.com"`, 1)
	raw = strings.Replace(raw, `"github.com/gm2211/grove"`, `"unrelated/cap"`, 1)
	a := New(&fakeWhoIs{answers: map[string]string{"100.64.0.1": raw}},
		[]config.TailscaleGrant{{Match: "giulio@example.com", Scopes: []string{"operator"}}})
	if res, ok := a.Authenticate(context.Background(), "100.64.0.1:1"); ok {
		t.Fatalf("login grant matched a tagged node: %+v", res)
	}
}

func TestAuthenticateCaches(t *testing.T) {
	whois := &fakeWhoIs{answers: map[string]string{"100.64.0.1": taggedWhoIs}}
	a := New(whois, nil)
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		a.Authenticate(context.Background(), "100.64.0.1:1")
	}
	if whois.calls != 1 {
		t.Fatalf("whois calls = %d, want 1 within the cache TTL", whois.calls)
	}
	now = now.Add(cacheTTL + time.Second)
	a.Authenticate(context.Background(), "100.64.0.1:1")
	if whois.calls != 2 {
		t.Fatalf("whois calls = %d, want a fresh lookup after the TTL", whois.calls)
	}
}

type fakeRunner struct{ args []string }

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	f.args = append([]string{name}, args...)
	return taggedWhoIs, "", nil
}

func TestCLIWhoIs(t *testing.T) {
	r := &fakeRunner{}
	id, err := CLIWhoIs{Runner: r, Bin: "/usr/bin/tailscale"}.WhoIs(context.Background(), netip.MustParseAddr("100.64.0.1"))
	if err != nil || id.StableID != "nARGOS" {
		t.Fatalf("WhoIs = %+v, %v", id, err)
	}
	if strings.Join(r.args, " ") != "/usr/bin/tailscale whois --json 100.64.0.1" {
		t.Fatalf("ran %v", r.args)
	}
}
