package broker

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func newEntry(t *testing.T, job string, kind Kind, token string) Entry {
	t.Helper()
	ph, err := NewPlaceholder()
	if err != nil {
		t.Fatal(err)
	}
	return Entry{Placeholder: ph, Job: job, Kind: kind, Token: token, Expires: time.Now().Add(time.Hour)}
}

func TestNewPlaceholder(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		ph, err := NewPlaceholder()
		if err != nil {
			t.Fatal(err)
		}
		if len(ph) != len(PlaceholderPrefix)+52 || !strings.HasPrefix(ph, PlaceholderPrefix) || placeholderRE.FindString(ph) != ph {
			t.Fatalf("placeholder %q is malformed", ph)
		}
		if seen[ph] {
			t.Fatalf("placeholder %q repeated", ph)
		}
		seen[ph] = true
	}
}

func TestStoreReplaceValidates(t *testing.T) {
	good := newEntry(t, "j", KindGitHub, "ghs_abc")
	for name, mutate := range map[string]func(*Entry){
		"short placeholder":    func(e *Entry) { e.Placeholder = e.Placeholder[:20] },
		"padded placeholder":   func(e *Entry) { e.Placeholder = "x" + e.Placeholder },
		"upper placeholder":    func(e *Entry) { e.Placeholder = strings.ToUpper(e.Placeholder) },
		"unknown kind":         func(e *Entry) { e.Kind = "gitlab" },
		"no token":             func(e *Entry) { e.Token = "" },
		"token with newline":   func(e *Entry) { e.Token = "ghs_a\r\nX-Evil: 1" },
		"token with space":     func(e *Entry) { e.Token = "ghs a" },
		"token with non-ascii": func(e *Entry) { e.Token = "ghs_é" },
	} {
		e := good
		mutate(&e)
		s := NewStore(0)
		if err := s.Replace([]Entry{e}); err == nil {
			t.Fatalf("%s: Replace accepted %+v", name, e)
		}
	}
	s := NewStore(1)
	if err := s.Replace([]Entry{good, newEntry(t, "k", KindGitHub, "t2")}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("over the cap: err = %v, want ErrTooMany", err)
	}
	if err := s.Replace([]Entry{good}); err != nil || s.Len() != 1 {
		t.Fatalf("Replace(good) = %v, Len %d", err, s.Len())
	}
}

func TestStoreReplaceDropsMissing(t *testing.T) {
	a, b := newEntry(t, "a", KindGitHub, "ta"), newEntry(t, "b", KindGitHub, "tb")
	s := NewStore(0)
	if err := s.Replace([]Entry{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace([]Entry{b}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(a.Placeholder, "github.com", netip.Addr{}); !errors.Is(err, ErrUnknownPlaceholder) {
		t.Fatalf("finished job's entry still served: %v", err)
	}
	// A failed Replace leaves the old set alone.
	if err := s.Replace([]Entry{{Placeholder: "bad"}}); err == nil {
		t.Fatal("bad Replace accepted")
	}
	if _, err := s.Lookup(b.Placeholder, "github.com", netip.Addr{}); err != nil {
		t.Fatalf("failed Replace dropped entries: %v", err)
	}
}

func TestStoreLookup(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	vm := netip.MustParseAddr("192.168.64.5")
	gh := newEntry(t, "j", KindGitHub, "ghs_real")
	gh.Expires = now.Add(time.Minute)
	bound := newEntry(t, "j", KindAnthropic, "sk-ant-real")
	bound.Expires = now.Add(time.Minute)
	bound.Source = vm
	old := newEntry(t, "j", KindOpenAI, "old")
	old.Expires = now.Add(-time.Second)

	s := NewStore(0)
	s.now = func() time.Time { return now }
	if err := s.Replace([]Entry{gh, bound, old}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		ph   string
		host string
		src  netip.Addr
		want error
	}{
		{"github on api.github.com", gh.Placeholder, "api.github.com", vm, nil},
		{"github on github.com", gh.Placeholder, "github.com", netip.Addr{}, nil},
		{"github token to anthropic", gh.Placeholder, "api.anthropic.com", vm, ErrWrongHost},
		{"github token to a look-alike", gh.Placeholder, "github.com.evil.test", vm, ErrWrongHost},
		{"bound from its VM", bound.Placeholder, "api.anthropic.com", vm, nil},
		{"bound from its VM, v4-mapped", bound.Placeholder, "api.anthropic.com", netip.MustParseAddr("::ffff:192.168.64.5"), nil},
		{"bound from another VM", bound.Placeholder, "api.anthropic.com", netip.MustParseAddr("192.168.64.6"), ErrWrongSource},
		{"expired", old.Placeholder, "api.openai.com", vm, ErrUnknownPlaceholder},
		{"unknown", "gph1_" + strings.Repeat("a", 52), "api.github.com", vm, ErrUnknownPlaceholder},
	}
	for _, c := range cases {
		e, err := s.Lookup(c.ph, c.host, c.src)
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if c.want == nil && e.Token == "" {
			t.Fatalf("%s: no token", c.name)
		}
		if c.want != nil && e.Token != "" {
			t.Fatalf("%s: error carried a token", c.name)
		}
	}

	if n := s.Sweep(); n != 1 || s.Len() != 2 {
		t.Fatalf("Sweep = %d, Len = %d; want 1 dropped, 2 left", n, s.Len())
	}
}

func TestStoreDropVM(t *testing.T) {
	a, b := newEntry(t, "a", KindGitHub, "ta"), newEntry(t, "b", KindGitHub, "tb")
	a.VM, b.VM = "vm-1", "vm-2"
	s := NewStore(0)
	if err := s.Replace([]Entry{a, b}); err != nil {
		t.Fatal(err)
	}
	if n := s.DropVM("vm-1"); n != 1 || s.Len() != 1 {
		t.Fatalf("DropVM = %d, Len = %d", n, s.Len())
	}
}

func TestInjectHostsAreFixed(t *testing.T) {
	got := strings.Join(InjectHosts(), ",")
	want := "api.anthropic.com,api.github.com,api.openai.com,chatgpt.com,github.com"
	if got != want {
		t.Fatalf("InjectHosts = %s, want %s", got, want)
	}
}
