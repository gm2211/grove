package broker

import (
	"encoding/base64"
	"net/http"
	"net/netip"
	"strings"
)

// injectHeaders are the only request headers a placeholder is swapped in. Bodies, URLs and every
// other header are passed through untouched, so a job can't make an upstream store or echo a real
// token by putting its placeholder somewhere the upstream keeps (an issue body, a commit, a URL).
var injectHeaders = []string{"Authorization", "X-Api-Key"}

// inject swaps every placeholder in req's injectHeaders for the real token behind it, looked up
// for host (the upstream the request goes to) and src (the job VM's address). It returns the
// tokens it put in, so the response can be stripped of them. A placeholder the store doesn't
// know, or knows but not for this host or source, fails the whole request: nothing is sent.
//
// HTTP Basic credentials are decoded first, because git sends "x-access-token:<token>" that way.
func inject(req *http.Request, host string, src netip.Addr, lookup func(placeholder, host string, src netip.Addr) (Entry, error)) ([]string, error) {
	var tokens []string
	swap := func(s string) (string, error) {
		var err error
		out := placeholderRE.ReplaceAllStringFunc(s, func(ph string) string {
			if err != nil {
				return ph
			}
			var e Entry
			if e, err = lookup(ph, host, src); err != nil {
				return ph
			}
			tokens = append(tokens, e.Token)
			return e.Token
		})
		return out, err
	}
	for _, name := range injectHeaders {
		values := req.Header.Values(name)
		if len(values) == 0 {
			continue
		}
		swapped := make([]string, len(values))
		for i, v := range values {
			if scheme, cred, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Basic") && name == "Authorization" {
				if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred)); err == nil && placeholderRE.Match(raw) {
					out, err := swap(string(raw))
					if err != nil {
						return nil, err
					}
					swapped[i] = scheme + " " + base64.StdEncoding.EncodeToString([]byte(out))
					continue
				}
			}
			out, err := swap(v)
			if err != nil {
				return nil, err
			}
			swapped[i] = out
		}
		req.Header.Del(name)
		for _, v := range swapped {
			req.Header.Add(name, v)
		}
	}
	return tokens, nil
}
