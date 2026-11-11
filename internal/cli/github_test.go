package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/gitauth"
)

func TestPrintGitHubSourcingStatus(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	render := func(status gitauth.Status) string {
		var out bytes.Buffer
		printGitHubSourcingStatus(&out, &status)
		return out.String()
	}
	base := gitauth.Status{Enabled: true, Hosts: []string{"github.com"}, Repos: []string{"gm2211/grove"}, TokenFingerprint: "abc", ExpiresAt: &expires}

	if got := render(base); !strings.Contains(got, "operators only") {
		t.Fatalf("no allow list should read as operators only:\n%s", got)
	}
	withAllow := base
	withAllow.Allow = []string{"dev_laptop", "tag:argos"}
	if got := render(withAllow); !strings.Contains(got, "operators, dev_laptop, tag:argos") {
		t.Fatalf("allow list missing:\n%s", got)
	}
	legacy := base
	legacy.Legacy, legacy.Warning = true, "re-arm it"
	if got := render(legacy); !strings.Contains(got, "ANY submitter") || !strings.Contains(got, "WARNING:     re-arm it") {
		t.Fatalf("legacy credential not called out:\n%s", got)
	}
}

// --ttl has a default and --allow exists: arming from the CLI can't ask for "forever".
func TestGitHubEnableFlags(t *testing.T) {
	enable, _, err := Root.Find([]string{"github", "enable"})
	if err != nil {
		t.Fatal(err)
	}
	if got := enable.Flags().Lookup("ttl").DefValue; got != gitauth.DefaultTTL.String() {
		t.Fatalf("--ttl default = %q, want %s", got, gitauth.DefaultTTL)
	}
	if enable.Flags().Lookup("allow") == nil {
		t.Fatal("grove github enable has no --allow")
	}
}

func TestReadEnableToken(t *testing.T) {
	t.Run("from stdin", func(t *testing.T) {
		token, err := readEnableToken(strings.NewReader("ghp_secret\n"), "", true)
		if err != nil || token != "ghp_secret" {
			t.Fatalf("readEnableToken = %q, %v", token, err)
		}
	})
	t.Run("empty stdin is an error", func(t *testing.T) {
		if _, err := readEnableToken(strings.NewReader("  \n"), "", true); err == nil {
			t.Fatal("empty stdin was accepted")
		}
	})
	t.Run("from the named environment variable", func(t *testing.T) {
		t.Setenv("GROVE_TEST_TOKEN", " ghp_secret ")
		token, err := readEnableToken(nil, "GROVE_TEST_TOKEN", false)
		if err != nil || token != "ghp_secret" {
			t.Fatalf("readEnableToken = %q, %v", token, err)
		}
	})
	t.Run("unset environment variable names itself in the error", func(t *testing.T) {
		t.Setenv("GROVE_TEST_TOKEN", "")
		_, err := readEnableToken(nil, "GROVE_TEST_TOKEN", false)
		if err == nil || !strings.Contains(err.Error(), "GROVE_TEST_TOKEN") {
			t.Fatalf("err = %v", err)
		}
	})
}
