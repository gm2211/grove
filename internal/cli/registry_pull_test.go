package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/registryauth"
	"github.com/gm2211/grove/internal/server"
)

func TestRegistryPullCommandsTakeNoTokenArgument(t *testing.T) {
	pull, _, err := registryCmd.Find([]string{"pull"})
	if err != nil || pull == registryCmd {
		t.Fatalf("grove registry pull is not registered: %v", err)
	}
	for _, name := range []string{"enable", "disable", "status", "scope"} {
		sub, _, err := pull.Find([]string{name})
		if err != nil || sub == pull {
			t.Fatalf("grove registry pull %s is not registered: %v", name, err)
		}
		for _, flag := range []string{"token", "password", "token-env"} {
			if sub.Flags().Lookup(flag) != nil {
				t.Errorf("grove registry pull %s has a --%s flag: a credential in argv shows up in ps and shell history", name, flag)
			}
		}
	}
	enable, _, _ := pull.Find([]string{"enable"})
	for _, flag := range []string{"user", "repo", "registry"} {
		if enable.Flags().Lookup(flag) == nil {
			t.Errorf("enable has no --%s", flag)
		}
	}
}

func TestPrintRegistryPullStatusShowsAFingerprintNotAToken(t *testing.T) {
	var out bytes.Buffer
	printRegistryPullStatus(&out, &registryauth.Status{
		Enabled: true, Registry: "ghcr.io", User: "gm2211", Repos: []string{"gm2211/argos-ci"},
		TokenFingerprint: registryauth.Fingerprint("ghp_secret"),
	})
	text := out.String()
	for _, want := range []string{"ghcr.io", "gm2211", "gm2211/argos-ci", registryauth.Fingerprint("ghp_secret")} {
		if !strings.Contains(text, want) {
			t.Errorf("status output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ghp_secret") {
		t.Errorf("status output holds the token:\n%s", text)
	}
	out.Reset()
	printRegistryPullStatus(&out, &registryauth.Status{})
	if !strings.Contains(out.String(), "off") {
		t.Errorf("disarmed status = %q", out.String())
	}
}

type fakePullFetcher struct {
	login *server.VMImagePullLogin
	err   error
}

func (f fakePullFetcher) BrokerVMImagePull(context.Context) (*server.VMImagePullLogin, error) {
	return f.login, f.err
}

func TestPrintVMImagePullLogin(t *testing.T) {
	var out bytes.Buffer
	err := printVMImagePullLogin(context.Background(), &out, fakePullFetcher{login: &server.VMImagePullLogin{Registry: "ghcr.io", User: "gm2211", Token: "ghp_tok"}})
	if err != nil || out.String() != "gm2211\nghp_tok\n" {
		t.Fatalf("out = %q, err = %v", out.String(), err)
	}
	for name, f := range map[string]fakePullFetcher{
		"nothing for this worker": {},
		"control plane down":      {err: errors.New("dial tcp: refused")},
		"newline in the token":    {login: &server.VMImagePullLogin{User: "u", Token: "a\nb"}},
	} {
		out.Reset()
		if err := printVMImagePullLogin(context.Background(), &out, f); err == nil || out.Len() != 0 {
			t.Errorf("%s: want an error and no output, got %q, %v", name, out.String(), err)
		}
	}
}

func TestPullLoginCommandIsHiddenAndTakesNoCredentialFlag(t *testing.T) {
	sub, _, err := brokerCmd.Find([]string{"pull-login"})
	if err != nil || sub == brokerCmd {
		t.Fatalf("broker pull-login is not registered: %v", err)
	}
	if !sub.Hidden {
		t.Error("pull-login is plumbing for the worker wrapper and should be hidden")
	}
	for _, flag := range []string{"token", "password", "user"} {
		if sub.Flags().Lookup(flag) != nil {
			t.Errorf("pull-login has a --%s flag", flag)
		}
	}
}
