package nomad

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNodeTokens_PolicyIsNodeOnlyAndTokensAreScoped(t *testing.T) {
	var mu sync.Mutex
	var policy struct{ Name, Rules string }
	var created struct {
		Name, Type string
		Policies   []string
	}
	var deleted []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if got := r.Header.Get("X-Nomad-Token"); got != "mgmt" {
			t.Errorf("%s %s sent token %q, want the management token", r.Method, r.URL.Path, got)
		}
		switch {
		case (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.URL.Path == "/v1/acl/policy/"+NodeTokenPolicy:
			_ = json.NewDecoder(r.Body).Decode(&policy)
		case (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.URL.Path == "/v1/acl/token":
			_ = json.NewDecoder(r.Body).Decode(&created)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessorID": "acc-1", "SecretID": "sec-1", "Name": created.Name,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/acl/tokens":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"AccessorID": "acc-1", "Name": NodeTokenPrefix + "linux-mac1-0"},
				{"AccessorID": "op-1", "Name": "operator laptop"},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/acl/token/"):
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/v1/acl/token/"))
			if strings.HasSuffix(r.URL.Path, "/missing") {
				http.Error(w, "ACL token not found", http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte("true"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, "mgmt")
	if err != nil {
		t.Fatal(err)
	}
	tokens, ok := AsNodeTokens(c)
	if !ok {
		t.Fatal("concrete client should implement NodeTokens")
	}
	ctx := context.Background()

	tok, err := tokens.CreateNodeToken(ctx, "linux-mac1-0")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessorID != "acc-1" || tok.SecretID != "sec-1" {
		t.Fatalf("token = %+v", tok)
	}
	if created.Type != "client" || strings.Join(created.Policies, ",") != NodeTokenPolicy || created.Name != NodeTokenPrefix+"linux-mac1-0" {
		t.Errorf("created token = %+v, want a client token with only %s", created, NodeTokenPolicy)
	}
	if !strings.Contains(policy.Rules, "node {\n  policy = \"write\"\n}") ||
		!strings.Contains(policy.Rules, "agent {\n  policy = \"read\"\n}") ||
		strings.Contains(policy.Rules, "namespace") || strings.Contains(policy.Rules, "operator") {
		t.Errorf("node policy must grant node:write and agent:read (for -self) and no namespace capability:\n%s", policy.Rules)
	}

	listed, err := tokens.ListNodeTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].AccessorID != "acc-1" {
		t.Errorf("ListNodeTokens = %+v, want only grove's VM token", listed)
	}

	if err := tokens.RevokeNodeToken(ctx, "acc-1"); err != nil {
		t.Fatal(err)
	}
	if err := tokens.RevokeNodeToken(ctx, "missing"); err != nil {
		t.Errorf("revoking an already-gone token should succeed, got %v", err)
	}
	if strings.Join(deleted, ",") != "acc-1,missing" {
		t.Errorf("deleted = %v", deleted)
	}
}
