package server

import (
	"bufio"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin requiredScope and safeArtifactPath to their Lean transcriptions in
// spec/lean (Grove/Auth.lean, Grove/ArtifactPath.lean), where S5, S6, S7 and S9 are proved: if
// the Go code changes and the Lean model doesn't (or the other way round), they fail. The Lean
// side runs as spec/lean's grove-spec binary, named by GROVE_SPEC_BIN (CI's spec job builds it);
// without it they skip.

func specBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("GROVE_SPEC_BIN")
	if bin == "" {
		t.Skip("GROVE_SPEC_BIN not set: build spec/lean (lake build) and point it at .lake/build/bin/grove-spec")
	}
	return bin
}

// askLean sends every query to grove-spec in one run and returns its answers in order.
func askLean(t *testing.T, bin string, queries []string) []string {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader(strings.Join(queries, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("grove-spec: %v", err)
	}
	var answers []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		answers = append(answers, sc.Text())
	}
	if len(answers) != len(queries) {
		t.Fatalf("grove-spec answered %d of %d queries", len(answers), len(queries))
	}
	return answers
}

// specPaths is what an attacker might put in a path: every registered route with hostile
// parameters, plus random strings over the characters that matter to requiredScope.
func specPaths(rng *rand.Rand) []string {
	params := []string{"", "x", "mac1", "mac-2.local", "snooze", "wake", "cancel", "credentials",
		"..", ".", "a/b", "../secrets", "mac1/snooze", "é", "MAC_1", strings.Repeat("a", 129), "whoami"}
	templates := []string{"/fleet", "/fleet/reconcile", "/vms/%s/recycle", "/workers/%s/pause",
		"/workers/%s/resume", "/workers/%s/snooze", "/workers/%s/wake", "/jobs", "/jobs/%s",
		"/jobs/%s/cancel", "/jobs/%s/credentials", "/jobs/%s/logs", "/jobs/%s/artifacts/%s", "/healthz",
		"/join/requests", "/join/requests/%s", "/join/pending", "/join/approve/%s", "/access/devices",
		"/access/devices/%s", "/whoami", "/secrets", "/secrets/%s", "/github/sourcing", "/broker/entries",
		"/access", "/workers/%s", "/workers/%s/snooze/%s", "%s/broker/entries"}
	var paths []string
	for _, tpl := range templates {
		n := strings.Count(tpl, "%s")
		for _, a := range params {
			for _, b := range params[:4] {
				args := []any{a, b}[:n]
				paths = append(paths, fmt.Sprintf(tpl, args...))
			}
		}
	}
	alphabet := []string{"/", "workers", "jobs", "snooze", "wake", "cancel", "credentials", "access",
		"secrets", "broker", "entries", "github", "vms", "join", "approve", "pending", "whoami", "a", ".", "..", "-", "é"}
	for range 3000 {
		var b strings.Builder
		for range 1 + rng.Intn(8) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		paths = append(paths, b.String())
	}
	return paths
}

func TestSpec_RequiredScopeMatchesLean(t *testing.T) {
	bin := specBinary(t)
	paths := specPaths(rand.New(rand.NewSource(1)))
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	var queries, want []string
	for _, p := range paths {
		if strings.ContainsAny(p, "\t\n") {
			continue
		}
		for _, m := range methods {
			r := &http.Request{Method: m, URL: &url.URL{Path: p}}
			queries = append(queries, "scope\t"+m+"\t"+p)
			want = append(want, requiredScope(r))
		}
	}
	got := askLean(t, bin, queries)
	for i := range queries {
		if got[i] != want[i] {
			t.Errorf("%q: Go requiredScope = %q, Lean = %q", queries[i], want[i], got[i])
		}
	}
	t.Logf("%d (method, path) pairs agree", len(queries))
}

func TestSpec_SafeArtifactPathMatchesLean(t *testing.T) {
	bin := specBinary(t)
	rng := rand.New(rand.NewSource(2))
	alphabet := []string{"/", "a", "b.txt", ".", "..", "\\", "é", "-", "out"}
	var queries []string
	var want []bool
	for range 5000 {
		var b strings.Builder
		for range rng.Intn(7) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		queries = append(queries, "artifact\t"+b.String())
		want = append(want, safeArtifactPath(b.String()))
	}
	got := askLean(t, bin, queries)
	for i := range queries {
		if (got[i] == "ok") != want[i] {
			t.Errorf("%q: Go safeArtifactPath = %v, Lean = %s", queries[i], want[i], got[i])
		}
	}
}

// The Lean proofs are about the path requiredScope sees. This is the assumption that makes
// them about grove: however a read-only caller spells a path to an operator route (dot
// segments, encoded slashes, doubled slashes), the operator handler never answers it.
func TestSpec_NoSpellingOfAnOperatorRouteGetsPastAuth(t *testing.T) {
	store, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := store.Issue("reader", []string{ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(nil, nil, nil, Options{Token: "operator-token", AccessStore: store})
	get := func(raw, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://grove.test/", nil)
		req.URL = &url.URL{Path: "", RawPath: ""}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		req.URL = u
		req.RequestURI = raw
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	for _, target := range []string{"/access/devices", "/join/pending"} {
		if rec := get("/api/v1"+target, "operator-token"); rec.Code != http.StatusOK {
			t.Fatalf("baseline: operator GET %s = %d, want 200", target, rec.Code)
		}
		if rec := get("/api/v1"+target, reader); rec.Code != http.StatusForbidden {
			t.Fatalf("baseline: reader GET %s = %d, want 403", target, rec.Code)
		}
		rest := strings.TrimPrefix(target, "/")
		for _, spelling := range []string{
			"/api/v1/jobs/../" + rest,
			"/api/v1/jobs/x/../../" + rest,
			"/api/v1/./" + rest,
			"/api/v1//" + rest,
			"/api/v1/jobs/x%2F..%2F..%2F" + strings.ReplaceAll(rest, "/", "%2F"),
			"/api/v1/jobs%2F..%2F" + strings.ReplaceAll(rest, "/", "%2F"),
			"/api/v1/fleet/..%2F" + rest,
			"/api/v1/" + strings.ToUpper(rest[:1]) + rest[1:],
		} {
			rec := get(spelling, reader)
			if rec.Code == http.StatusOK {
				t.Errorf("reader GET %s = 200 (%s): an operator route answered a read-only credential", spelling, strings.TrimSpace(rec.Body.String()))
			}
		}
	}
}
