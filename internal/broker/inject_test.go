package broker

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestInject(t *testing.T) {
	gh := newEntry(t, "j", KindGitHub, "ghs_REALGITHUB")
	ant := newEntry(t, "j", KindAnthropic, "sk-ant-oat01-REAL")
	s := NewStore(0)
	if err := s.Replace([]Entry{gh, ant}); err != nil {
		t.Fatal(err)
	}
	src := netip.MustParseAddr("192.168.64.5")
	basic := func(user, pass string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}

	cases := []struct {
		name    string
		host    string
		header  http.Header
		want    http.Header
		tokens  []string
		wantErr error
	}{
		{
			name:   "bearer",
			host:   "api.github.com",
			header: http.Header{"Authorization": {"Bearer " + gh.Placeholder}},
			want:   http.Header{"Authorization": {"Bearer ghs_REALGITHUB"}},
			tokens: []string{"ghs_REALGITHUB"},
		},
		{
			name:   "token scheme",
			host:   "api.github.com",
			header: http.Header{"Authorization": {"token " + gh.Placeholder}},
			want:   http.Header{"Authorization": {"token ghs_REALGITHUB"}},
			tokens: []string{"ghs_REALGITHUB"},
		},
		{
			name:   "git basic auth",
			host:   "github.com",
			header: http.Header{"Authorization": {basic("x-access-token", gh.Placeholder)}},
			want:   http.Header{"Authorization": {basic("x-access-token", "ghs_REALGITHUB")}},
			tokens: []string{"ghs_REALGITHUB"},
		},
		{
			name:   "x-api-key",
			host:   "api.anthropic.com",
			header: http.Header{"X-Api-Key": {ant.Placeholder}},
			want:   http.Header{"X-Api-Key": {"sk-ant-oat01-REAL"}},
			tokens: []string{"sk-ant-oat01-REAL"},
		},
		{
			name:   "other headers are never swapped",
			host:   "api.github.com",
			header: http.Header{"X-Echo": {gh.Placeholder}, "Cookie": {"a=" + gh.Placeholder}},
			want:   http.Header{"X-Echo": {gh.Placeholder}, "Cookie": {"a=" + gh.Placeholder}},
		},
		{
			name:   "no placeholder, nothing to do",
			host:   "api.github.com",
			header: http.Header{"Authorization": {"Bearer ghp_jobsown"}},
			want:   http.Header{"Authorization": {"Bearer ghp_jobsown"}},
		},
		{
			name:    "github placeholder sent to anthropic",
			host:    "api.anthropic.com",
			header:  http.Header{"Authorization": {"Bearer " + gh.Placeholder}},
			wantErr: ErrWrongHost,
		},
		{
			name:    "github placeholder in basic auth sent to anthropic",
			host:    "api.anthropic.com",
			header:  http.Header{"Authorization": {basic("x", gh.Placeholder)}},
			wantErr: ErrWrongHost,
		},
		{
			name:    "unknown placeholder",
			host:    "api.github.com",
			header:  http.Header{"Authorization": {"Bearer gph1_" + strings.Repeat("q", 52)}},
			wantErr: ErrUnknownPlaceholder,
		},
		{
			name:    "one good, one unknown",
			host:    "api.github.com",
			header:  http.Header{"Authorization": {"Bearer " + gh.Placeholder}, "X-Api-Key": {"gph1_" + strings.Repeat("q", 52)}},
			wantErr: ErrUnknownPlaceholder,
		},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodGet, "https://"+c.host+"/", nil)
		req.Header = c.header.Clone()
		tokens, err := inject(req, c.host, src, s.Lookup)
		if !errors.Is(err, c.wantErr) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.wantErr)
		}
		if err != nil {
			if strings.Contains(err.Error(), "REAL") {
				t.Fatalf("%s: error carries a token: %v", c.name, err)
			}
			continue
		}
		for k := range c.want {
			if got, want := strings.Join(req.Header.Values(k), "|"), strings.Join(c.want.Values(k), "|"); got != want {
				t.Fatalf("%s: %s = %q, want %q", c.name, k, got, want)
			}
		}
		if strings.Join(tokens, ",") != strings.Join(c.tokens, ",") {
			t.Fatalf("%s: tokens = %v, want %v", c.name, tokens, c.tokens)
		}
	}
}
