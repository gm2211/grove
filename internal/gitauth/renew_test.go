package gitauth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/grant"
)

func armed(t *testing.T, renew string) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "github.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enable(EnableOptions{Token: "ghp_firsttokenfirsttokenfirsttoken000000", Repos: []string{"acme/app"}, Allow: []string{"dev1"}, TTL: 10 * time.Hour, By: "operator", Renew: renew}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRenew_KeepsScopeAndRestartsTheLifetime(t *testing.T) {
	s := armed(t, RenewFromGH)
	now := time.Now()
	if s.RenewDue(now) {
		t.Fatal("due right after arming")
	}
	later := now.Add(5*time.Hour + time.Minute)
	if !s.RenewDue(later) {
		t.Fatal("not due after half its lifetime")
	}
	before := s.Status()
	st, err := s.Renew("ghp_secondtokensecondtokensecondtoken000", later)
	if err != nil {
		t.Fatal(err)
	}
	if st.TokenFingerprint == before.TokenFingerprint {
		t.Error("token not replaced")
	}
	if got := st.ExpiresAt.Sub(later).Round(time.Minute); got != 10*time.Hour {
		t.Errorf("new lifetime %s, want 10h", got)
	}
	if len(st.Repos) != 1 || st.Repos[0] != "acme/app" || len(st.Allow) != 1 || st.EnabledBy != "operator" || st.Renew != RenewFromGH {
		t.Errorf("scope changed: %+v", st)
	}
	if tok, ok := s.TokenFor("https://github.com/acme/app", grant.Caller{Operator: true}); !ok || tok != "ghp_secondtokensecondtokensecondtoken000" {
		t.Errorf("TokenFor after renew = %q %t", tok, ok)
	}
	if s.RenewDue(later) {
		t.Error("still due right after renewing")
	}
}

func TestRenew_NeverArmsAnything(t *testing.T) {
	// A credential armed without --renew lapses as before and can't be renewed.
	s := armed(t, "")
	if s.RenewDue(time.Now().Add(9 * time.Hour)) {
		t.Error("a credential without renew reported due")
	}
	if _, err := s.Renew("ghp_secondtokensecondtokensecondtoken000", time.Now()); err == nil {
		t.Error("renewed a credential that wasn't armed to renew")
	}
	// A lapsed one stays lapsed.
	s = armed(t, RenewFromGH)
	after := time.Now().Add(11 * time.Hour)
	if s.RenewDue(after) {
		t.Error("a lapsed credential reported due")
	}
	if _, err := s.Renew("ghp_secondtokensecondtokensecondtoken000", after); err == nil {
		t.Error("renewed a lapsed credential")
	}
	if s.Status().Enabled {
		t.Error("lapsed credential still enabled")
	}
	// Disarmed: nothing to renew.
	s = armed(t, RenewFromGH)
	if _, err := s.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Renew("ghp_secondtokensecondtokensecondtoken000", time.Now()); err == nil {
		t.Error("renewed a disarmed credential")
	}
}

func TestEnable_RefusesAnUnknownRenewSource(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "github.json"))
	if _, err := s.Enable(EnableOptions{Token: "ghp_firsttokenfirsttokenfirsttoken000000", Repos: []string{"acme/app"}, Renew: "keychain"}); err == nil {
		t.Error("accepted renew=keychain")
	}
}
