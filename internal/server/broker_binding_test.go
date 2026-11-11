package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/tsauth"
)

// fakeTailnet is a PeerAuthenticator that grants nothing and names the node behind each address.
type fakeTailnet struct {
	nodes map[string]tsauth.Identity // by IP
}

func (f *fakeTailnet) Authenticate(context.Context, string) (tsauth.Result, bool) {
	return tsauth.Result{}, false
}

func (f *fakeTailnet) Identify(_ context.Context, remoteAddr string) (tsauth.Identity, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	id, ok := f.nodes[host]
	return id, ok
}

type joinHarness struct {
	t      *testing.T
	srv    *Server
	access *AccessStore
	oc     *fakeOrchard
	ds     *fakeDispatch
	tail   *fakeTailnet
	logs   *bytes.Buffer
}

func newJoinHarness(t *testing.T) *joinHarness {
	t.Helper()
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &joinHarness{t: t, access: access, oc: &fakeOrchard{}, logs: &bytes.Buffer{},
		ds:   &fakeDispatch{jobs: map[string]*dispatch.Job{}, brokerEntries: map[string][]broker.Entry{}},
		tail: &fakeTailnet{nodes: map[string]tsauth.Identity{}}}
	h.srv = newTestServer(h.oc, nil, h.ds, Options{
		Token: "operator", AccessStore: access, Tailscale: h.tail,
		Logger:               slog.New(slog.NewTextHandler(h.logs, nil)),
		IssueWorkerBootstrap: func(context.Context, string) (string, error) { return "worker-bootstrap", nil },
	})
	return h
}

type joinHandle struct{ ID, Secret, Code string }

// request posts a join request from ip and returns the recorder.
func (h *joinHarness) request(name, ip string, brokerOnly bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"name": name, "tailnetIp": ip, "brokerOnly": brokerOnly})
	return peerRequest(h.srv, http.MethodPost, "/api/v1/join/requests", string(body), net.JoinHostPort(ip, "40000"), "")
}

func (h *joinHarness) create(name, ip string, brokerOnly bool) joinHandle {
	h.t.Helper()
	rec := h.request(name, ip, brokerOnly)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create %s from %s: %d %s", name, ip, rec.Code, rec.Body)
	}
	var out joinHandle
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *joinHarness) approve(code string, replace bool) *httptest.ResponseRecorder {
	path := "/api/v1/join/approve/" + code
	if replace {
		path += "?replace=true"
	}
	return peerRequest(h.srv, http.MethodPost, path, "", "127.0.0.1:1", "operator")
}

type joinDelivery struct {
	Status, BootstrapToken, ClientToken, DeviceID, BrokerToken, BrokerDeviceID string
}

func (h *joinHarness) poll(j joinHandle, phase string) (int, joinDelivery) {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/join/requests/"+j.ID, nil)
	req.Header.Set("X-Grove-Join-Secret", j.Secret)
	if phase != "" {
		req.Header.Set("X-Grove-Join-Phase", phase)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	var out joinDelivery
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// join runs a whole approved join and returns what the joining machine received.
func (h *joinHarness) join(name, ip string, replace bool) joinDelivery {
	h.t.Helper()
	j := h.create(name, ip, false)
	if rec := h.approve(j.Code, replace); rec.Code != http.StatusOK {
		h.t.Fatalf("approve %s from %s: %d %s", name, ip, rec.Code, rec.Body)
	}
	code, out := h.poll(j, "")
	if code != http.StatusOK || out.BrokerToken == "" {
		h.t.Fatalf("poll %s: %d %+v", name, code, out)
	}
	return out
}

func (h *joinHarness) entries(remote, token string) *httptest.ResponseRecorder {
	return peerRequest(h.srv, http.MethodGet, "/api/v1/broker/entries", "", remote, token)
}

func (h *joinHarness) liveBroker(name string) int {
	return len(h.access.BrokerBindings(name))
}

// M1 (1): the operator token cannot mint a broker credential, through the API or the store.
func TestBrokerScopeIsIssuedOnlyByJoinApproval(t *testing.T) {
	h := newJoinHarness(t)
	for _, body := range []string{
		`{"name":"mac-1","scopes":["broker"]}`,
		`{"name":"mac-1","scopes":["read","broker"]}`,
		`{"name":"mac-1","scopes":[" broker "]}`,
		`{"name":"mac-1","scopes":["broker"],"expiresIn":"1h"}`,
	} {
		rec := peerRequest(h.srv, http.MethodPost, "/api/v1/access/devices", body, "127.0.0.1:1", "operator")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "join approval") {
			t.Errorf("POST /access/devices %s: %d %s, want 403 naming join approval", body, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), `"token"`) {
			t.Errorf("POST /access/devices %s returned a token", body)
		}
	}
	if _, _, err := h.access.Issue("mac-1", []string{ScopeBroker}); !errors.Is(err, ErrBrokerScopeNotIssuable) {
		t.Errorf("Issue(broker) = %v, want ErrBrokerScopeNotIssuable", err)
	}
	if _, _, err := h.access.IssueExpiring("mac-1", []string{ScopeBroker}, 1e9); !errors.Is(err, ErrBrokerScopeNotIssuable) {
		t.Errorf("IssueExpiring(broker) = %v, want ErrBrokerScopeNotIssuable", err)
	}
	if _, _, err := h.access.IssueJoined("mac-1", []string{ScopeBroker}); !errors.Is(err, ErrBrokerScopeNotIssuable) {
		t.Errorf("IssueJoined(broker) = %v, want ErrBrokerScopeNotIssuable (a broker credential needs a binding)", err)
	}
	if _, _, err := h.access.IssueJoinedBound("mac-1", []string{ScopeBroker}, Binding{}); err == nil {
		t.Error("IssueJoinedBound with no address succeeded")
	}
	if n := len(h.access.List()); n != 0 {
		t.Fatalf("%d credentials were stored, want none", n)
	}
	// Other scopes still issue.
	rec := peerRequest(h.srv, http.MethodPost, "/api/v1/access/devices", `{"name":"ci","scopes":["read","dispatch"]}`, "127.0.0.1:1", "operator")
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue read+dispatch: %d %s", rec.Code, rec.Body)
	}
}

// M1 (3): a join-issued broker credential works only from the machine that joined.
func TestBrokerCredentialIsBoundToTheJoiningMachine(t *testing.T) {
	h := newJoinHarness(t)
	h.tail.nodes["100.64.0.20"] = tsauth.Identity{StableID: "nMAC1", Node: "mac-1.tail1234.ts.net"}
	h.tail.nodes["fd7a:115c:a1e0::20"] = tsauth.Identity{StableID: "nMAC1", Node: "mac-1.tail1234.ts.net"}
	h.tail.nodes["100.64.0.66"] = tsauth.Identity{StableID: "nEVIL", Node: "laptop.tail1234.ts.net"}
	h.ds.brokerEntries["mac-1"] = []broker.Entry{brokerEntry(t, "job-1", "ghs_one")}

	got := h.join("mac-1", "100.64.0.20", false)
	if b, ok := h.access.Binding(got.BrokerDeviceID); !ok || b.IP != "100.64.0.20" || b.Node != "nMAC1" {
		t.Fatalf("binding = %+v, want the joining address and node", b)
	}

	if rec := h.entries("100.64.0.20:5123", got.BrokerToken); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ghs_one") {
		t.Fatalf("from the joining Mac: %d %s", rec.Code, rec.Body)
	}
	// The same node over its other tailnet address is the same machine.
	if rec := h.entries("[fd7a:115c:a1e0::20]:5123", got.BrokerToken); rec.Code != http.StatusOK {
		t.Fatalf("same node, IPv6 address: %d %s", rec.Code, rec.Body)
	}
	for name, remote := range map[string]string{
		"another tailnet node":       "100.64.0.66:5123",
		"an address nobody can name": "100.64.0.77:5123",
		"loopback":                   "127.0.0.1:5123",
	} {
		rec := h.entries(remote, got.BrokerToken)
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "ghs_") {
			t.Errorf("%s: %d %s, want 403 and no token", name, rec.Code, rec.Body)
		}
	}

	// The local socket has no peer address: a bound credential is refused there.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/broker/entries", nil)
	req.Header.Set("Authorization", "Bearer "+got.BrokerToken)
	req = req.WithContext(LocalConnContext(req.Context(), nil))
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("local socket: %d %s, want 403", rec.Code, rec.Body)
	}

	// The address re-assigned to another node: the node id decides, not the address.
	h.tail.nodes["100.64.0.20"] = tsauth.Identity{StableID: "nOTHER", Node: "reused.tail1234.ts.net"}
	if rec := h.entries("100.64.0.20:5123", got.BrokerToken); rec.Code != http.StatusForbidden {
		t.Errorf("joining address now held by another node: %d, want 403", rec.Code)
	}
	// Tailscale unable to name the peer: the verified join address decides.
	delete(h.tail.nodes, "100.64.0.20")
	if rec := h.entries("100.64.0.20:5123", got.BrokerToken); rec.Code != http.StatusOK {
		t.Errorf("whois unavailable, joining address: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := h.entries("100.64.0.66:5123", got.BrokerToken); rec.Code != http.StatusForbidden {
		t.Errorf("whois names another node: %d, want 403", rec.Code)
	}
}

// A worker on the control plane's own Mac reaches `grove serve` from loopback (or this host's own
// tailnet address): its credential is bound to that, with no Tailscale node to name.
func TestBrokerCredentialBoundToLoopbackForTheControlPlanesOwnWorker(t *testing.T) {
	h := newJoinHarness(t)
	j := h.create("studio", "127.0.0.1", true)
	if rec := h.approve(j.Code, false); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	_, got := h.poll(j, "")
	if b, _ := h.access.Binding(got.BrokerDeviceID); b.IP != "127.0.0.1" || b.Node != "" {
		t.Fatalf("binding = %+v, want loopback and no node", b)
	}
	if rec := h.entries("127.0.0.1:999", got.BrokerToken); rec.Code != http.StatusOK {
		t.Fatalf("from loopback: %d %s", rec.Code, rec.Body)
	}
	if rec := h.entries("100.64.0.66:999", got.BrokerToken); rec.Code != http.StatusForbidden {
		t.Fatalf("from the tailnet: %d, want 403", rec.Code)
	}
}

// A credential from before bindings existed (devices.json has no boundIp) keeps working from
// anywhere, and says so once.
func TestUnboundLegacyBrokerCredentialStillWorksAndWarnsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	const token = "legacy-token"
	legacy := `{"version":1,"credentials":[{"id":"aaaa","name":"mac-1","scopes":["broker"],"source":"join",` +
		`"createdAt":"2026-01-01T00:00:00Z","tokenHash":"` + hashToken(token) + `"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	access, err := NewAccessStore(path)
	if err != nil {
		t.Fatalf("an older devices.json must still load: %v", err)
	}
	var logs bytes.Buffer
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{Token: "operator", AccessStore: access, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	for _, remote := range []string{"100.64.0.20:1", "100.64.0.99:1", "127.0.0.1:1"} {
		if rec := peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", remote, token); rec.Code != http.StatusOK {
			t.Fatalf("legacy credential from %s: %d %s", remote, rec.Code, rec.Body)
		}
	}
	if n := strings.Count(logs.String(), "not bound to its machine"); n != 1 {
		t.Fatalf("unbound warning logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "grove setup") {
		t.Errorf("the warning should say to run `grove setup` again:\n%s", logs.String())
	}
}

// M1 (2): a join that uses an existing worker's name from another machine needs --replace.
func TestJoinUnderAnExistingWorkersNameNeedsReplace(t *testing.T) {
	h := newJoinHarness(t)
	h.tail.nodes["100.64.0.20"] = tsauth.Identity{StableID: "nMAC1", Node: "mac-1.tail1234.ts.net"}
	h.tail.nodes["100.64.0.66"] = tsauth.Identity{StableID: "nEVIL", Node: "laptop.tail1234.ts.net"}

	first := h.join("mac-1", "100.64.0.20", false) // a new name: nothing to replace
	h.oc.workers = []orchard.Worker{{Name: "mac-1"}, {Name: "old-mac"}}

	// Another machine asks for mac-1's name.
	evil := h.create("mac-1", "100.64.0.66", false)
	rec := peerRequest(h.srv, http.MethodGet, "/api/v1/join/pending", "", "127.0.0.1:1", "operator")
	var pending []pendingJoin
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v %s", err, rec.Body)
	}
	if p := pending[0]; p.TailnetIP != "100.64.0.66" || p.Node != "laptop.tail1234.ts.net" || !p.ReplacesWorker {
		t.Fatalf("pending = %+v, want the asking address, its node name and replacesWorker", p)
	}
	rec = h.approve(evil.Code, false)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "100.64.0.66") || !strings.Contains(rec.Body.String(), "--replace") {
		t.Fatalf("approve without replace: %d %s, want 409 naming the address and --replace", rec.Code, rec.Body)
	}
	if code, _ := h.poll(evil, ""); code != http.StatusAccepted {
		t.Fatalf("refused request poll = %d, want still pending", code)
	}
	if n := h.liveBroker("mac-1"); n != 1 {
		t.Fatalf("%d live broker credentials for mac-1 after a refused approval, want 1", n)
	}

	// The same Mac running `grove setup` again is not a replacement.
	again := h.join("mac-1", "100.64.0.20", false)
	if again.BrokerToken == first.BrokerToken {
		t.Fatal("re-join returned the same token")
	}

	// An explicit override goes through.
	if rec := h.approve(evil.Code, true); rec.Code != http.StatusOK {
		t.Fatalf("approve with replace: %d %s", rec.Code, rec.Body)
	}

	// A worker Orchard lists that has no broker credential at all (it enrolled before the
	// broker) has nothing to compare with: also an override.
	old := h.create("old-mac", "100.64.0.20", false)
	if rec := h.approve(old.Code, false); rec.Code != http.StatusConflict {
		t.Fatalf("existing worker with no credential: %d, want 409", rec.Code)
	}

	// Orchard unreachable: the name can't be shown free, so no approval without the override.
	h.oc.listErr = errors.New("orchard down")
	fresh := h.create("brand-new", "100.64.0.20", false)
	if rec := h.approve(fresh.Code, false); rec.Code != http.StatusBadGateway {
		t.Fatalf("orchard down: %d, want 502", rec.Code)
	}
	if rec := h.approve(fresh.Code, true); rec.Code != http.StatusOK {
		t.Fatalf("orchard down, with replace: %d %s", rec.Code, rec.Body)
	}
}

// An unbound (pre-binding) credential can't show the asker is the same Mac, so the first
// `grove setup` after the upgrade needs --replace; it then retires the unbound credential.
func TestJoinOverAnUnboundCredentialNeedsReplaceOnceAndRetiresIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	legacy := `{"version":1,"credentials":[{"id":"aaaa","name":"mac-1","scopes":["broker"],` +
		`"createdAt":"2026-01-01T00:00:00Z","tokenHash":"` + hashToken("legacy") + `"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newJoinHarness(t)
	access, err := NewAccessStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h.access = access
	h.srv = newTestServer(h.oc, nil, h.ds, Options{Token: "operator", AccessStore: access,
		IssueWorkerBootstrap: func(context.Context, string) (string, error) { return "b", nil }})
	h.oc.workers = []orchard.Worker{{Name: "mac-1"}}

	j := h.create("mac-1", "100.64.0.20", false)
	if rec := h.approve(j.Code, false); rec.Code != http.StatusConflict {
		t.Fatalf("approve over an unbound credential: %d, want 409", rec.Code)
	}
	if rec := h.approve(j.Code, true); rec.Code != http.StatusOK {
		t.Fatalf("approve with replace: %d %s", rec.Code, rec.Body)
	}
	h.oc.workers = []orchard.Worker{{Name: "mac-1", LastSeen: time.Now().Add(time.Minute)}}
	if code, out := h.poll(j, "verify"); code != http.StatusOK || out.Status != "online" {
		t.Fatalf("verify: %d %+v", code, out)
	}
	if _, ok := access.Authenticate("legacy"); ok {
		t.Fatal("the unbound credential should be revoked once the bound one is enrolled")
	}
	// From now on the same Mac re-joins with no override.
	h.join("mac-1", "100.64.0.20", false)
}

// A broker-only join hands out the broker credential alone, and needs no Orchard issuer.
func TestBrokerOnlyJoin(t *testing.T) {
	h := newJoinHarness(t)
	h.srv = newTestServer(h.oc, nil, h.ds, Options{Token: "operator", AccessStore: h.access})
	h.oc.workers = []orchard.Worker{{Name: "studio"}}

	rec := h.request("studio", "127.0.0.1", true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"brokerOnly":true`) {
		t.Fatalf("create: %d %s, want the brokerOnly echo", rec.Code, rec.Body)
	}
	var j joinHandle
	_ = json.Unmarshal(rec.Body.Bytes(), &j)
	if rec := h.approve(j.Code, false); rec.Code != http.StatusConflict {
		t.Fatalf("existing worker, no replace: %d, want 409", rec.Code)
	}
	if rec := h.approve(j.Code, true); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	code, got := h.poll(j, "")
	if code != http.StatusOK || got.BrokerToken == "" || got.BootstrapToken != "" || got.ClientToken != "" {
		t.Fatalf("delivery = %d %+v, want only a broker credential", code, got)
	}
	if p, ok := h.access.Authenticate(got.BrokerToken); !ok || p.Name != "studio" || len(p.Scopes) != 1 || p.Scopes[0] != ScopeBroker {
		t.Fatalf("credential = %+v", p)
	}
	if n := len(h.access.List()); n != 1 {
		t.Fatalf("%d credentials issued, want only the broker one", n)
	}

	// A second one supersedes the first once collected.
	j2 := h.create("studio", "127.0.0.1", true)
	if rec := h.approve(j2.Code, false); rec.Code != http.StatusOK {
		t.Fatalf("same machine again: %d %s", rec.Code, rec.Body)
	}
	_, got2 := h.poll(j2, "")
	if code, _ := h.poll(j2, "verify"); code != http.StatusOK {
		t.Fatalf("acknowledge: %d", code)
	}
	if _, ok := h.access.Authenticate(got.BrokerToken); ok {
		t.Error("the earlier broker credential should be revoked")
	}
	if _, ok := h.access.Authenticate(got2.BrokerToken); !ok {
		t.Error("the new broker credential must stay live")
	}
	if code, _ := h.poll(j2, ""); code != http.StatusNotFound {
		t.Errorf("acknowledged request retained: %d", code)
	}
}
