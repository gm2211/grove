package registryauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeRegistry is an OCI distribution registry with token auth: the manifest endpoint answers 401
// with a Bearer challenge until it sees the token the token endpoint hands out for a covered
// login (or, for public repositories, to anyone).
type fakeRegistry struct {
	*httptest.Server
	mu sync.Mutex
	// private repositories (path) need the login user:token at the token endpoint.
	private map[string]bool
	user    string
	token   string
	// digests maps "path:tag" to the digest the manifest HEAD answers; missing → 404.
	digests map[string]string
	// noDigestHeader makes a successful HEAD omit Docker-Content-Digest.
	noDigestHeader bool
	// realm overrides the realm in the challenge (default: this server's /token).
	realm string
	// tokenStatus, when set, is the token endpoint's status code.
	tokenStatus int
	// basicChallenge makes the manifest endpoint ask for Basic instead of Bearer.
	basicChallenge bool
	// redirectManifest makes the manifest endpoint answer 302 to this URL.
	redirectManifest string

	tokenRequests    []string // Authorization header of each token request
	manifestRequests []string // Authorization header of each manifest request
	accept           string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{private: map[string]bool{}, digests: map[string]string{}, user: "gm2211", token: testToken}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.URL, "https://") }

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		f.tokenRequests = append(f.tokenRequests, r.Header.Get("Authorization"))
		if f.tokenStatus != 0 {
			http.Error(w, "denied", f.tokenStatus)
			return
		}
		scope := r.URL.Query().Get("scope")
		path := strings.TrimSuffix(strings.TrimPrefix(scope, "repository:"), ":pull")
		u, p, hasAuth := r.BasicAuth()
		granted := "anonymous"
		if hasAuth && u == f.user && p == f.token {
			granted = "good"
		}
		if f.private[path] && granted != "good" {
			granted = "anonymous" // the token exists but grants nothing on this repository
		}
		_, _ = w.Write([]byte(`{"token":"` + granted + `"}`))
		return
	}
	if r.Method != http.MethodHead || !strings.Contains(r.URL.Path, "/manifests/") {
		http.NotFound(w, r)
		return
	}
	f.manifestRequests = append(f.manifestRequests, r.Header.Get("Authorization"))
	f.accept = r.Header.Get("Accept")
	if f.redirectManifest != "" {
		http.Redirect(w, r, f.redirectManifest, http.StatusFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	path, tag, _ := strings.Cut(rest, "/manifests/")
	auth := r.Header.Get("Authorization")
	authorized := auth == "Bearer good" || (!f.private[path] && auth == "Bearer anonymous")
	if f.basicChallenge {
		authorized = auth != ""
	}
	if !authorized {
		challenge := `Bearer realm="` + f.URL + `/token",service="test-registry"`
		if f.realm != "" {
			challenge = `Bearer realm="` + f.realm + `",service="test-registry"`
		}
		if f.basicChallenge {
			challenge = `Basic realm="test-registry"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	digest, ok := f.digests[path+":"+tag]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !f.noDigestHeader {
		w.Header().Set("Docker-Content-Digest", digest)
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeRegistry) resolver(t *testing.T, repos ...string) *Resolver {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "registry-pull.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) > 0 {
		if _, err := store.Enable(EnableOptions{Registry: f.host(), User: f.user, Token: f.token, Repos: repos}); err != nil {
			t.Fatal(err)
		}
	}
	return &Resolver{Store: store, Client: f.Client()}
}

func TestResolveTag_PrivateRepositoryWithTheStoredCredential(t *testing.T) {
	f := newFakeRegistry(t)
	f.private["gm2211/argos-ci"] = true
	f.digests["gm2211/argos-ci:v1"] = testDigest
	ref := f.host() + "/gm2211/argos-ci:v1"

	got, err := f.resolver(t, "gm2211/argos-ci").ResolveTag(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.host() + "/gm2211/argos-ci@" + testDigest; got != want {
		t.Fatalf("pinned = %q, want %q", got, want)
	}
	// The login went to the token endpoint, as Basic; the manifest only ever saw the Bearer token.
	if len(f.tokenRequests) != 1 || !strings.HasPrefix(f.tokenRequests[0], "Basic ") {
		t.Fatalf("token requests = %v, want one Basic login", f.tokenRequests)
	}
	for _, auth := range f.manifestRequests {
		if strings.Contains(auth, testToken) || strings.HasPrefix(auth, "Basic ") {
			t.Errorf("the manifest request carried the login: %q", auth)
		}
	}
	for _, mediaType := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"} {
		if !strings.Contains(f.accept, mediaType) {
			t.Errorf("Accept = %q, missing %s", f.accept, mediaType)
		}
	}
}

func TestResolveTag_PublicRepositoryIsAnonymous(t *testing.T) {
	f := newFakeRegistry(t)
	f.digests["acme/public:latest"] = testDigest
	got, err := f.resolver(t).ResolveTag(context.Background(), f.host()+"/acme/public:latest")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "@"+testDigest) {
		t.Fatalf("pinned = %q", got)
	}
	if len(f.tokenRequests) != 1 || f.tokenRequests[0] != "" {
		t.Fatalf("an uncovered repository must be fetched without credentials, token requests = %v", f.tokenRequests)
	}
}

func TestResolveTag_FailuresNameTheImageAndNeverFallBack(t *testing.T) {
	ctx := context.Background()
	t.Run("private repository, no covering credential", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.private["gm2211/argos-ci"] = true
		f.digests["gm2211/argos-ci:v1"] = testDigest
		got, err := f.resolver(t, "gm2211/other").ResolveTag(ctx, f.host()+"/gm2211/argos-ci:v1")
		if err == nil || got != "" {
			t.Fatalf("resolved %q, %v; want a failure", got, err)
		}
		if !strings.Contains(err.Error(), "gm2211/argos-ci:v1") {
			t.Errorf("error does not name the image: %v", err)
		}
	})
	t.Run("disarmed store", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.private["gm2211/argos-ci"] = true
		f.digests["gm2211/argos-ci:v1"] = testDigest
		if got, err := f.resolver(t).ResolveTag(ctx, f.host()+"/gm2211/argos-ci:v1"); err == nil || got != "" {
			t.Fatalf("resolved %q, %v; want a failure", got, err)
		}
	})
	t.Run("token endpoint refuses", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.private["gm2211/argos-ci"] = true
		f.tokenStatus = http.StatusForbidden
		_, err := f.resolver(t, "gm2211/argos-ci").ResolveTag(ctx, f.host()+"/gm2211/argos-ci:v1")
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("err = %v, want a refusal that does not leak the token", err)
		}
	})
	t.Run("unknown tag", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.private["gm2211/argos-ci"] = true
		_, err := f.resolver(t, "gm2211/argos-ci").ResolveTag(ctx, f.host()+"/gm2211/argos-ci:nope")
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("err = %v, want the registry's 404", err)
		}
	})
	t.Run("no Docker-Content-Digest", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.digests["acme/public:latest"] = testDigest
		f.noDigestHeader = true
		if _, err := f.resolver(t).ResolveTag(ctx, f.host()+"/acme/public:latest"); err == nil {
			t.Fatal("a response with no digest header was accepted")
		}
	})
	t.Run("malformed digest", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.digests["acme/public:latest"] = "sha256:short"
		if _, err := f.resolver(t).ResolveTag(ctx, f.host()+"/acme/public:latest"); err == nil {
			t.Fatal("a malformed digest was accepted")
		}
	})
	t.Run("redirects are not followed", func(t *testing.T) {
		f := newFakeRegistry(t)
		other := newFakeRegistry(t)
		other.digests["acme/public:latest"] = testDigest
		f.redirectManifest = other.URL + "/v2/acme/public/manifests/latest"
		if _, err := f.resolver(t).ResolveTag(ctx, f.host()+"/acme/public:latest"); err == nil {
			t.Fatal("a redirect was followed")
		}
		if len(other.manifestRequests) != 0 {
			t.Fatal("the redirect target was contacted")
		}
	})
}

func TestResolveTag_NeverSendsTheLoginToAnotherHost(t *testing.T) {
	f := newFakeRegistry(t)
	f.private["gm2211/argos-ci"] = true
	f.digests["gm2211/argos-ci:v1"] = testDigest
	thief := newFakeRegistry(t) // a token endpoint somewhere else
	f.realm = thief.URL + "/token"
	_, err := f.resolver(t, "gm2211/argos-ci").ResolveTag(context.Background(), f.host()+"/gm2211/argos-ci:v1")
	if err == nil {
		t.Fatal("a challenge pointing at another host was followed")
	}
	if len(thief.tokenRequests) != 0 {
		t.Fatalf("the other host saw token requests %v", thief.tokenRequests)
	}
	f.realm = "http://" + f.host() + "/token" // plain http, same host
	if _, err := f.resolver(t, "gm2211/argos-ci").ResolveTag(context.Background(), f.host()+"/gm2211/argos-ci:v1"); err == nil {
		t.Fatal("a plain-http realm was followed")
	}
}

func TestResolveTag_BasicChallenge(t *testing.T) {
	f := newFakeRegistry(t)
	f.basicChallenge = true
	f.digests["gm2211/argos-ci:v1"] = testDigest
	got, err := f.resolver(t, "gm2211/argos-ci").ResolveTag(context.Background(), f.host()+"/gm2211/argos-ci:v1")
	if err != nil || !strings.HasSuffix(got, "@"+testDigest) {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := f.resolver(t).ResolveTag(context.Background(), f.host()+"/gm2211/argos-ci:v1"); err == nil {
		t.Fatal("a Basic challenge was answered with no credential")
	}
}

func TestResolveTag_RefusesWhatIsNotATagReference(t *testing.T) {
	r := &Resolver{Store: nil}
	for _, ref := range []string{
		"", "ghcr.io/gm2211/argos-ci", "ghcr.io/gm2211/argos-ci@" + testDigest, "argos-ci:v1",
		"ghcr.io/gm2211/argos-ci:", "ghcr.io/gm2211/argos-ci:bad tag", "gm2211/argos-ci:v1",
	} {
		if got, err := r.ResolveTag(context.Background(), ref); err == nil || got != "" {
			t.Errorf("ResolveTag(%q) = %q, %v; want a refusal", ref, got, err)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, p := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:gm2211/argos-ci:pull"`)
	if scheme != "Bearer" || p["realm"] != "https://ghcr.io/token" || p["service"] != "ghcr.io" || p["scope"] != "repository:gm2211/argos-ci:pull" {
		t.Fatalf("scheme %q params %v", scheme, p)
	}
	if scheme, p := parseChallenge(`Basic realm="x"`); scheme != "Basic" || p["realm"] != "x" {
		t.Fatalf("scheme %q params %v", scheme, p)
	}
	if scheme, p := parseChallenge(""); scheme != "" || len(p) != 0 {
		t.Fatalf("scheme %q params %v", scheme, p)
	}
}
