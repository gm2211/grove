package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetOperatorToken(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{Token: "old"})
	status := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if status("old") != http.StatusOK || status("new") != http.StatusUnauthorized {
		t.Fatal("the configured token should be the one in force")
	}
	if err := srv.SetOperatorToken(""); err == nil {
		t.Fatal("an empty operator token was accepted")
	}
	if status("old") != http.StatusOK {
		t.Fatal("a refused replacement dropped the token in force")
	}
	if err := srv.SetOperatorToken("new"); err != nil {
		t.Fatal(err)
	}
	if status("old") != http.StatusUnauthorized || status("new") != http.StatusOK {
		t.Fatal("after the replacement only the new token should work")
	}
}
