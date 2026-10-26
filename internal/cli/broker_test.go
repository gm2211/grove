package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrokerFetchReadsItsCredentialOnceFound(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/broker/entries" {
			http.NotFound(w, r)
			return
		}
		auth = append(auth, r.Header.Get("Authorization"))
		w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	reads := 0
	stored := false
	fetch := brokerFetch(srv.URL, func(context.Context) (string, error) {
		reads++
		if !stored {
			return "", errors.New("no broker credential in Keychain")
		}
		return "broker-token", nil
	})
	ctx := context.Background()
	if _, err := fetch(ctx); err == nil {
		t.Fatal("fetch without a credential succeeded")
	}
	if len(auth) != 0 {
		t.Fatal("called the control plane without a credential")
	}
	stored = true
	for i := 0; i < 3; i++ {
		if _, err := fetch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 {
		t.Errorf("Keychain read %d times, want once more after the credential appeared", reads)
	}
	for _, a := range auth {
		if a != "Bearer broker-token" {
			t.Errorf("Authorization = %q", a)
		}
	}
}
