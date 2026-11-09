package registryauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// manifestAccept lists the manifest media types a registry may answer a tag with: OCI image index
// and manifest, Docker manifest list and v2 manifest. Docker-Content-Digest of whichever it
// serves is the digest a pull of that tag resolves to.
const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

var (
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// maxTokenResponse bounds what Resolver reads from a registry's token endpoint.
const maxTokenResponse = 64 << 10

// Resolver turns "<registry>/<repository>:<tag>" into "<registry>/<repository>@sha256:<digest>"
// by asking the registry (OCI distribution API). When the armed credential covers the repository
// it authenticates the way the registry's WWW-Authenticate challenge asks; otherwise the request
// is anonymous, which is enough for a public image.
//
// The credential is only ever sent to the registry host it was armed for: a challenge whose token
// realm is another host, or not https, is refused, and redirects are never followed.
type Resolver struct {
	Store *Store
	// Client is the HTTP client to use; nil means one with a 20 second timeout. Its redirect
	// policy is replaced by "never follow".
	Client *http.Client
}

// ResolveTag implements dispatch.ImageResolver.
func (r *Resolver) ResolveTag(ctx context.Context, ref string) (string, error) {
	repoRef, tag, err := splitTagRef(ref)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	host, path, ok := SplitImage(repoRef)
	if !ok {
		return "", fmt.Errorf("resolve %q: no explicit registry host", ref)
	}
	client := r.httpClient()
	cred, hasCred := r.Store.CredentialFor(repoRef)
	digest, err := headManifest(ctx, client, host, path, tag, cred, hasCred)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	return repoRef + "@" + digest, nil
}

func (r *Resolver) httpClient() *http.Client {
	base := http.Client{Timeout: 20 * time.Second}
	if r != nil && r.Client != nil {
		base = *r.Client
	}
	base.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &base
}

// splitTagRef splits "<host>/<path>:<tag>" and requires an explicit tag.
func splitTagRef(ref string) (repo, tag string, err error) {
	if strings.Contains(ref, "@") {
		return "", "", errors.New("is already pinned by digest")
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return "", "", errors.New("has no tag (name <registry>/<repository>:<tag> or pin a digest)")
	}
	repo, tag = ref[:i], ref[i+1:]
	if !tagPattern.MatchString(tag) {
		return "", "", fmt.Errorf("tag %q is not a valid tag", tag)
	}
	if _, _, ok := SplitImage(repo); !ok {
		return "", "", errors.New("has no explicit registry host")
	}
	return repo, tag, nil
}

func headManifest(ctx context.Context, client *http.Client, host, path, tag string, cred Credential, hasCred bool) (string, error) {
	manifestURL := "https://" + host + "/v2/" + path + "/manifests/" + url.PathEscape(tag)
	resp, err := head(ctx, client, manifestURL, "")
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		auth, err := authorize(ctx, client, host, path, challenge, cred, hasCred)
		if err != nil {
			return "", err
		}
		if resp, err = head(ctx, client, manifestURL, auth); err != nil {
			return "", err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry %s answered %d for the manifest (is the tag there, and does the registry credential cover %s?)", host, resp.StatusCode, path)
	}
	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("registry %s sent no usable Docker-Content-Digest (%q)", host, digest)
	}
	return digest, nil
}

func head(ctx context.Context, client *http.Client, target, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request failed: %w", scrubURLError(err))
	}
	_ = resp.Body.Close()
	return resp, nil
}

// authorize answers a 401 challenge with an Authorization header value: a bearer token fetched
// from the challenge's realm, or Basic for a registry that asks for it. Without a covering
// credential it still asks the realm for an anonymous token.
func authorize(ctx context.Context, client *http.Client, host, path, challenge string, cred Credential, hasCred bool) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if !hasCred {
			return "", fmt.Errorf("registry %s wants credentials and none cover %s", host, path)
		}
		return basicHeader(cred), nil
	case "bearer":
	default:
		return "", fmt.Errorf("registry %s sent an unusable WWW-Authenticate challenge", host)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || !strings.EqualFold(realm.Host, host) {
		return "", fmt.Errorf("registry %s sent a token realm outside itself; refusing to send a credential there", host)
	}
	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + path + ":pull"
	}
	query.Set("scope", scope)
	realm.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if hasCred {
		req.Header.Set("Authorization", basicHeader(cred))
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry token request failed: %w", scrubURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry %s refused a pull token (%d)", host, resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponse)).Decode(&body); err != nil {
		return "", fmt.Errorf("registry %s sent an unreadable token response", host)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("registry %s sent no token", host)
	}
	return "Bearer " + token, nil
}

func basicHeader(c Credential) string {
	req := &http.Request{Header: http.Header{}}
	req.SetBasicAuth(c.User, c.Token)
	return req.Header.Get("Authorization")
}

// scrubURLError drops the URL from a client error: it can carry a query string and the realm.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (scheme string, params map[string]string) {
	params = map[string]string{}
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		after = strings.TrimSpace(after)
		var val string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			val, rest = after[1:1+end], after[end+2:]
		} else {
			val, rest, _ = strings.Cut(after, ",")
			rest = "," + rest
		}
		params[key] = val
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return scheme, params
}
