package ci

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/gitauth"
)

// GitHub is the slice of the GitHub REST API the watcher uses. Every call carries the token it is
// given, and the token is only ever sent to the repository's own GitHub API host.
type GitHub interface {
	// BranchHead returns the commit sha at the tip of branch.
	BranchHead(ctx context.Context, token string, repo Repo, branch string) (string, error)
	// OpenPullRequests lists the repository's open pull requests.
	OpenPullRequests(ctx context.Context, token string, repo Repo) ([]PullRequest, error)
	// File returns the contents of path at commit sha; ErrNotFound when it doesn't exist there.
	File(ctx context.Context, token string, repo Repo, path, sha string) ([]byte, error)
	// SetStatus creates a commit status on sha.
	SetStatus(ctx context.Context, token string, repo Repo, sha string, status CommitStatus) error
}

// ErrNotFound is GitHub's 404.
var ErrNotFound = errors.New("not found on GitHub")

// Repo is a watched repository: "owner/name" on host.
type Repo struct {
	Host     string
	FullName string
}

// CloneURL is the https URL grove jobs clone the repository from.
func (r Repo) CloneURL() string { return "https://" + r.Host + "/" + r.FullName }

// apiBase is the REST API root for the repository's host: api.github.com for github.com, and
// GitHub Enterprise's /api/v3 for any other host.
func (r Repo) apiBase() string {
	if r.Host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + r.Host + "/api/v3"
}

// PullRequest is an open pull request's head.
type PullRequest struct {
	Number int
	// HeadSHA is the pull request's newest commit.
	HeadSHA string
	// HeadRepo is the "owner/name" the pull request's branch lives in; it differs from the base
	// repository for a pull request from a fork.
	HeadRepo string
}

// CommitStatus is the body of a commit status. State is one of pending, success, failure, error.
type CommitStatus struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

// HTTPGitHub is the GitHub implementation over net/http. It sends conditional requests for the
// reads it repeats every poll (branch heads, open pull requests): GitHub answers an unchanged one
// with 304, which does not count against the token's rate limit.
type HTTPGitHub struct {
	Client *http.Client
	// APIBase, when set, replaces every repository's API root (tests point it at a fake).
	APIBase string

	mu    sync.Mutex
	etags map[string]cachedResponse
}

type cachedResponse struct {
	etag string
	body []byte
}

// NewHTTPGitHub returns an HTTPGitHub with a bounded request timeout.
func NewHTTPGitHub() *HTTPGitHub {
	return &HTTPGitHub{Client: &http.Client{Timeout: 30 * time.Second}, etags: map[string]cachedResponse{}}
}

func (g *HTTPGitHub) BranchHead(ctx context.Context, token string, repo Repo, branch string) (string, error) {
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	u := g.base(repo) + "/repos/" + repo.FullName + "/branches/" + url.PathEscape(branch)
	if err := g.getJSON(ctx, token, u, true, &out); err != nil {
		return "", err
	}
	if out.Commit.SHA == "" {
		return "", fmt.Errorf("GitHub returned no commit for branch %q of %s", branch, repo.FullName)
	}
	return out.Commit.SHA, nil
}

func (g *HTTPGitHub) OpenPullRequests(ctx context.Context, token string, repo Repo) ([]PullRequest, error) {
	var page []struct {
		Number int `json:"number"`
		Head   struct {
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	// The 100 most recently updated: enough for any repository a fleet of Macs builds for.
	u := g.base(repo) + "/repos/" + repo.FullName + "/pulls?state=open&sort=updated&direction=desc&per_page=100"
	if err := g.getJSON(ctx, token, u, true, &page); err != nil {
		return nil, err
	}
	prs := make([]PullRequest, 0, len(page))
	for _, p := range page {
		pr := PullRequest{Number: p.Number, HeadSHA: p.Head.SHA}
		// A deleted fork has no head repository; it is not this repository either way.
		if p.Head.Repo != nil {
			pr.HeadRepo = p.Head.Repo.FullName
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

func (g *HTTPGitHub) File(ctx context.Context, token string, repo Repo, path, sha string) ([]byte, error) {
	var out struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	u := g.base(repo) + "/repos/" + repo.FullName + "/contents/" + path + "?ref=" + url.QueryEscape(sha)
	if err := g.getJSON(ctx, token, u, false, &out); err != nil {
		return nil, err
	}
	if out.Type != "file" || out.Encoding != "base64" {
		return nil, fmt.Errorf("%s at %s is not a regular file", path, sha)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

func (g *HTTPGitHub) SetStatus(ctx context.Context, token string, repo Repo, sha string, status CommitStatus) error {
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	u := g.base(repo) + "/repos/" + repo.FullName + "/statuses/" + url.PathEscape(sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.do(req, token)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return apiError(resp)
	}
	return nil
}

func (g *HTTPGitHub) base(repo Repo) string {
	if g.APIBase != "" {
		return strings.TrimRight(g.APIBase, "/")
	}
	return repo.apiBase()
}

// getJSON GETs u and decodes it into out. With cache, the response's ETag is remembered and sent
// back next time, and a 304 decodes the remembered body.
func (g *HTTPGitHub) getJSON(ctx context.Context, token, u string, cache bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	// The cache is per token too: a re-armed token must not be answered from what another saw.
	key := gitauth.Fingerprint(token) + " " + u
	var cached cachedResponse
	if cache {
		g.mu.Lock()
		cached = g.etags[key]
		g.mu.Unlock()
		if cached.etag != "" {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	resp, err := g.do(req, token)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && cached.etag != "":
		return json.Unmarshal(cached.body, out)
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return apiError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if cache {
		if etag := resp.Header.Get("ETag"); etag != "" {
			g.mu.Lock()
			if g.etags == nil {
				g.etags = map[string]cachedResponse{}
			}
			g.etags[key] = cachedResponse{etag: etag, body: body}
			g.mu.Unlock()
		}
	}
	return json.Unmarshal(body, out)
}

func (g *HTTPGitHub) do(req *http.Request, token string) (*http.Response, error) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "grove-ci")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http's error names the URL, never the Authorization header.
		return nil, err
	}
	return resp, nil
}

// apiError turns a non-2xx response into an error carrying GitHub's own message, which names
// what's wrong (a missing permission, a rate limit) without echoing the token.
func apiError(resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(data, &body)
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("GitHub %s %s: %d %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, msg)
}
