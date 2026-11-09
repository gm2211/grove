package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/registryauth"
)

func TestRegistryPullCommandsTakeNoTokenArgument(t *testing.T) {
	pull, _, err := registryCmd.Find([]string{"pull"})
	if err != nil || pull == registryCmd {
		t.Fatalf("grove registry pull is not registered: %v", err)
	}
	for _, name := range []string{"enable", "disable", "status"} {
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
