package cli

import (
	"strings"
	"testing"
)

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
