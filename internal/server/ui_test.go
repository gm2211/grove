package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The UI holds its bearer token in localStorage, so every UI response must carry a CSP that
// forbids inline/foreign script plus the framing/sniffing/referrer hardening headers.
func TestUIHandler_SetsSecurityHeaders(t *testing.T) {
	for _, path := range []string{"/", "/jobs/abc123", "/assets/index.js"} {
		rec := httptest.NewRecorder()
		UIHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		h := rec.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q missing %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP %q allows unsafe-*", path, csp)
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
		} {
			if got := h.Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
}

// The not-built placeholder is served under the same CSP, so it must not rely on inline styles.
func TestUINotBuiltPage_HasNoInlineStyleOrScript(t *testing.T) {
	for _, bad := range []string{"style=", "<style", "<script"} {
		if strings.Contains(uiNotBuiltPage, bad) {
			t.Errorf("uiNotBuiltPage contains %q, which the UI CSP blocks", bad)
		}
	}
}
