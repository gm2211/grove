package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/registryauth"
)

const vmPullToken = "ghp_vmImagePullSecret0123456789"

type vmPullFixture struct {
	srv    *Server
	store  *registryauth.Store
	logs   *bytes.Buffer
	broker string // broker credential of worker mac-1
	other  string // broker credential of worker mac-2
	device string // a plain dispatch device (what a job's submitter holds)
}

func newVMPullFixture(t *testing.T, images map[string][]string) *vmPullFixture {
	t.Helper()
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := registryauth.NewStore(filepath.Join(t.TempDir(), "registry-pull.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enable(registryauth.EnableOptions{User: "gm2211", Token: vmPullToken, Repos: []string{"gm2211/grove-macos-worker"}}); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	f := &vmPullFixture{store: store, logs: logs}
	f.srv = newTestServer(nil, nil, nil, Options{
		Token: "operator", AccessStore: access, RegistryAuth: store,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		WorkerVMImages: func(_ context.Context, worker string) ([]string, error) {
			if worker == "boom" {
				return nil, errors.New("no fleet")
			}
			return images[worker], nil
		},
	})
	if f.broker, _, err = access.IssueJoinedBound("mac-1", []string{ScopeBroker}, Binding{IP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if f.other, _, err = access.IssueJoinedBound("mac-2", []string{ScopeBroker}, Binding{IP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if f.device, _, err = access.Issue("ci", []string{ScopeDispatch, ScopeRead}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBrokerEntries_HandsTheVMImageLoginOnlyToTheWorkerForItsInScopeImages(t *testing.T) {
	f := newVMPullFixture(t, map[string][]string{
		"mac-1": {"ghcr.io/gm2211/grove-macos-worker@sha256:" + strings.Repeat("a", 64)},
		"mac-2": {"ghcr.io/someone/else@sha256:" + strings.Repeat("b", 64)},
	})
	path := "/api/v1/broker/entries?vmImagePull=1"

	rec := peerRequest(f.srv, http.MethodGet, path, "", "127.0.0.1:1", f.broker)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp BrokerEntriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.VMImagePull == nil || resp.VMImagePull.Registry != "ghcr.io" || resp.VMImagePull.User != "gm2211" || resp.VMImagePull.Token != vmPullToken {
		t.Fatalf("login = %+v", resp.VMImagePull)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the answer must be no-store")
	}

	// The broker's regular poll never carries it.
	rec = peerRequest(f.srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", f.broker)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), vmPullToken) {
		t.Fatalf("plain poll: %d %s", rec.Code, rec.Body)
	}

	// Another worker whose pools' images are outside the scope gets nothing.
	rec = peerRequest(f.srv, http.MethodGet, path, "", "127.0.0.1:1", f.other)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), vmPullToken) || strings.Contains(rec.Body.String(), "vmImagePull") {
		t.Fatalf("out-of-scope worker: %d %s", rec.Code, rec.Body)
	}

	// Not the operator token, and not a device a job's submitter holds.
	for name, token := range map[string]string{"operator": "operator", "dispatch device": f.device} {
		rec = peerRequest(f.srv, http.MethodGet, path, "", "127.0.0.1:1", token)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), vmPullToken) {
			t.Fatalf("%s: %d %s, want refused", name, rec.Code, rec.Body)
		}
	}
	// No credential at all.
	if rec = peerRequest(f.srv, http.MethodGet, path, "", "127.0.0.1:1", ""); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), vmPullToken) {
		t.Fatalf("anonymous: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(f.logs.String(), vmPullToken) {
		t.Fatalf("the token reached the log:\n%s", f.logs)
	}
}

func TestBrokerEntries_WithholdsTheVMImageLoginWhenAnImageIsOnAnotherHost(t *testing.T) {
	f := newVMPullFixture(t, map[string][]string{
		"mac-1": {
			"ghcr.io/gm2211/grove-macos-worker@sha256:" + strings.Repeat("a", 64),
			"registry.example.com/team/vm:latest", // Tart would present the login there too
		},
	})
	rec := peerRequest(f.srv, http.MethodGet, "/api/v1/broker/entries?vmImagePull=1", "", "127.0.0.1:1", f.broker)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), vmPullToken) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestBrokerEntries_LocalVMNamesDoNotBlockTheVMImageLogin(t *testing.T) {
	f := newVMPullFixture(t, map[string][]string{
		"mac-1": {"grove-studio-linux-v2", "ghcr.io/gm2211/grove-macos-worker@sha256:" + strings.Repeat("a", 64)},
	})
	rec := peerRequest(f.srv, http.MethodGet, "/api/v1/broker/entries?vmImagePull=1", "", "127.0.0.1:1", f.broker)
	var resp BrokerEntriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if resp.VMImagePull == nil || resp.VMImagePull.Token != vmPullToken {
		t.Fatalf("login = %+v, want it handed over beside a local VM name", resp.VMImagePull)
	}
}

func TestBrokerEntries_NoVMImageLoginWhenDisarmedOrFleetUnknown(t *testing.T) {
	f := newVMPullFixture(t, map[string][]string{"mac-1": {"ghcr.io/gm2211/grove-macos-worker@sha256:" + strings.Repeat("a", 64)}})
	path := "/api/v1/broker/entries?vmImagePull=1"
	if _, err := f.store.Disable(); err != nil {
		t.Fatal(err)
	}
	rec := peerRequest(f.srv, http.MethodGet, path, "", "127.0.0.1:1", f.broker)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "vmImagePull") {
		t.Fatalf("disarmed: %d %s", rec.Code, rec.Body)
	}
	// A server with no way to tell a worker's pools hands nothing.
	f.srv.opts.WorkerVMImages = nil
	rec = peerRequest(f.srv, http.MethodGet, path, "", "127.0.0.1:1", f.broker)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "vmImagePull") {
		t.Fatalf("no fleet hook: %d %s", rec.Code, rec.Body)
	}
}

func TestVMImagePullLogin_NeverPrintsTheToken(t *testing.T) {
	l := VMImagePullLogin{Registry: "ghcr.io", User: "u", Token: vmPullToken}
	for _, s := range []string{l.String(), l.GoString()} {
		if strings.Contains(s, vmPullToken) {
			t.Fatalf("leaked: %s", s)
		}
	}
}

func TestRegistryPullScope_EditsOnlyReposAndNeverEchoesTheToken(t *testing.T) {
	srv, store := newRegistryAuthServer(t, Options{})
	do(t, srv, http.MethodPut, "/api/v1/registry/pull", registryBody, "")
	before := store.Status().TokenFingerprint

	rec := do(t, srv, http.MethodPatch, "/api/v1/registry/pull", `{"add":["gm2211/grove-macos-worker"]}`, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "ghp_registrySecret") {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	st := decodeRegistryStatus(t, rec)
	if len(st.Repos) != 2 || st.TokenFingerprint != before {
		t.Fatalf("after add: %+v (fingerprint was %s)", st, before)
	}
	if _, ok := store.CredentialFor("ghcr.io/gm2211/grove-macos-worker@sha256:" + strings.Repeat("a", 64)); !ok {
		t.Fatal("added repo not covered")
	}

	rec = do(t, srv, http.MethodPatch, "/api/v1/registry/pull", `{"remove":["gm2211/argos-ci"]}`, "")
	if st = decodeRegistryStatus(t, rec); rec.Code != http.StatusOK || len(st.Repos) != 1 || st.Repos[0] != "gm2211/grove-macos-worker" {
		t.Fatalf("remove: %d %+v", rec.Code, st)
	}

	for name, body := range map[string]string{
		"a token in the body":    `{"add":["a/b"],"token":"ghp_other"}`,
		"nothing to change":      `{}`,
		"emptying the scope":     `{"remove":["gm2211/grove-macos-worker"]}`,
		"a malformed repository": `{"add":["nope"]}`,
	} {
		rec = do(t, srv, http.MethodPatch, "/api/v1/registry/pull", body, "")
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "ghp_other") {
			t.Fatalf("%s: %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	if got := store.Status(); len(got.Repos) != 1 || got.TokenFingerprint != before {
		t.Fatalf("a rejected edit changed the store: %+v", got)
	}
}

func TestRegistryPullScope_NeedsAnArmedCredentialAndTheOperator(t *testing.T) {
	srv, _ := newRegistryAuthServer(t, Options{})
	if rec := do(t, srv, http.MethodPatch, "/api/v1/registry/pull", `{"add":["a/b"]}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("disarmed: %d %s", rec.Code, rec.Body)
	}
	f := newVMPullFixture(t, nil)
	for name, token := range map[string]string{"dispatch device": f.device, "worker broker": f.broker} {
		if rec := do(t, f.srv, http.MethodPatch, "/api/v1/registry/pull", `{"add":["a/b"]}`, token); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d, want 403", name, rec.Code)
		}
	}
}
