package cli

import (
	"strings"
	"testing"
)

func TestReadRegistryToken(t *testing.T) {
	cases := []struct {
		name        string
		env, stdin  string
		interactive bool
		want        string
		wantErr     string
	}{
		{name: "env wins over stdin", env: " ghp_env\n", stdin: "ghp_stdin", want: "ghp_env"},
		{name: "piped stdin", stdin: "ghp_piped\n", want: "ghp_piped"},
		{name: "terminal is never read", stdin: "ghp_typed", interactive: true, wantErr: "pbpaste"},
		{name: "empty stdin", stdin: " \n", wantErr: "stdin was empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readRegistryToken(tc.env, strings.NewReader(tc.stdin), tc.interactive)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
