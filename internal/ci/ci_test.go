package ci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/dispatch"
)

const goodSpec = `
jobs:
  test:
    pool: linux
    timeout: 30m
    env: { CI: "1" }
    script: make test
  lint:
    pool: linux
    script: make lint
`

func TestParseSpec(t *testing.T) {
	spec, err := ParseSpec([]byte(goodSpec))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(spec.JobNames(), ","); got != "lint,test" {
		t.Fatalf("jobs = %s", got)
	}
	req := spec.Request("test", "https://github.com/o/r", "abc", map[string]string{"k": "v"})
	if req.Kind != dispatch.KindBuild || req.Pool != "linux" || req.Repo != "https://github.com/o/r" || req.Ref != "abc" ||
		req.Script != "make test" || req.Env["CI"] != "1" || req.Timeout.Duration() != 30*time.Minute ||
		req.Requester != Requester || req.IdempotencyKey != "grove-ci:https://github.com/o/r@abc:test" {
		t.Fatalf("request = %+v", req)
	}

	for name, bad := range map[string]string{
		"empty":         "",
		"no jobs":       "jobs: {}\n",
		"unknown key":   "jobs:\n  t:\n    pool: linux\n    script: x\n    image: evil\n",
		"no pool":       "jobs:\n  t:\n    script: x\n",
		"no script":     "jobs:\n  t:\n    pool: linux\n",
		"bad timeout":   "jobs:\n  t:\n    pool: linux\n    script: x\n    timeout: soon\n",
		"bad name":      "jobs:\n  'a b':\n    pool: linux\n    script: x\n",
		"not yaml":      "jobs: [",
		"top-level key": "on: push\njobs:\n  t:\n    pool: linux\n    script: x\n",
	} {
		if _, err := ParseSpec([]byte(bad)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// fakeGitHub serves branch heads, pull requests and files from memory and records statuses.
type fakeGitHub struct {
	mu       sync.Mutex
	branches map[string]string // branch -> sha
	prs      []PullRequest
	files    map[string]string // sha -> .grove.yml
	fileErr  error
	statuses []postedStatus
	tokens   []string
}

type postedStatus struct {
	sha string
	CommitStatus
}

func (f *fakeGitHub) BranchHead(_ context.Context, token string, _ Repo, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	sha, ok := f.branches[branch]
	if !ok {
		return "", ErrNotFound
	}
	return sha, nil
}

func (f *fakeGitHub) OpenPullRequests(context.Context, string, Repo) ([]PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PullRequest(nil), f.prs...), nil
}

func (f *fakeGitHub) File(_ context.Context, _ string, _ Repo, path, sha string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	data, ok := f.files[sha]
	if !ok || path != SpecPath {
		return nil, ErrNotFound
	}
	return []byte(data), nil
}

func (f *fakeGitHub) SetStatus(_ context.Context, _ string, _ Repo, sha string, status CommitStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, postedStatus{sha: sha, CommitStatus: status})
	return nil
}

func (f *fakeGitHub) posted() []postedStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postedStatus(nil), f.statuses...)
}

// fakeAuth covers the repositories named in it.
type fakeAuth map[string]string

func (a fakeAuth) TokenFor(repoURL string) (string, bool) {
	token, ok := a[repoURL]
	return token, ok
}

// fakeJobs is a dispatch.Service that keeps submitted jobs in memory, honouring idempotency keys.
type fakeJobs struct {
	dispatch.Service
	mu        sync.Mutex
	jobs      map[string]*dispatch.Job
	keys      map[string]string
	submitErr error
	submits   int
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{jobs: map[string]*dispatch.Job{}, keys: map[string]string{}}
}

func (f *fakeJobs) Submit(_ context.Context, req dispatch.JobRequest) (*dispatch.Job, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	if f.submitErr != nil {
		return nil, false, f.submitErr
	}
	if id, ok := f.keys[req.IdempotencyKey]; ok {
		job := *f.jobs[id]
		return &job, false, nil
	}
	id := fmt.Sprintf("job%d", len(f.jobs)+1)
	f.jobs[id] = &dispatch.Job{ID: id, Request: req, Status: dispatch.StatusPending}
	f.keys[req.IdempotencyKey] = id
	job := *f.jobs[id]
	return &job, true, nil
}

func (f *fakeJobs) Get(_ context.Context, id string) (*dispatch.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[id]
	if !ok {
		return nil, dispatch.ErrNotFound
	}
	cp := *job
	return &cp, nil
}

func (f *fakeJobs) set(id string, status dispatch.Status, exit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[id]
	job.Status = status
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	job.StartedAt, job.FinishedAt, job.ExitCode = &start, &end, &exit
}

func (f *fakeJobs) byName(name string) *dispatch.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, job := range f.jobs {
		if strings.HasSuffix(job.Request.IdempotencyKey, ":"+name) {
			return job
		}
	}
	return nil
}

const (
	repoURL = "https://github.com/o/r"
	mainSHA = "1111111111111111111111111111111111111111"
	prSHA   = "2222222222222222222222222222222222222222"
	forkSHA = "3333333333333333333333333333333333333333"
)

func newWatcher(t *testing.T, gh *fakeGitHub, jobs *fakeJobs) *Watcher {
	t.Helper()
	return &Watcher{
		Repos:     []config.CIRepo{{Repo: "o/r", Branches: []string{"main"}, PullRequests: true}},
		GitHub:    gh,
		Auth:      fakeAuth{repoURL: "tok"},
		Jobs:      jobs,
		ServerURL: "http://grove.example:6130/",
		StatePath: filepath.Join(t.TempDir(), "ci.json"),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestWatcher_RunsNewCommitsAndReportsResults(t *testing.T) {
	gh := &fakeGitHub{
		branches: map[string]string{"main": mainSHA},
		prs: []PullRequest{
			{Number: 7, HeadSHA: prSHA, HeadRepo: "o/r"},
			{Number: 8, HeadSHA: forkSHA, HeadRepo: "stranger/r"},
			{Number: 9, HeadSHA: mainSHA, HeadRepo: "o/r"}, // same commit as main: runs once
		},
		files: map[string]string{mainSHA: goodSpec, prSHA: goodSpec, forkSHA: goodSpec},
	}
	jobs := newFakeJobs()
	w := newWatcher(t, gh, jobs)
	ctx := context.Background()

	w.Tick(ctx)
	if jobs.submits != 4 {
		t.Fatalf("submits = %d, want 2 jobs × 2 commits (the fork's pull request never runs)", jobs.submits)
	}
	for _, job := range jobs.jobs {
		if job.Request.Ref == forkSHA {
			t.Fatalf("started a job for a fork's pull request: %+v", job.Request)
		}
		if job.Request.Repo != repoURL || job.Request.Meta["ci.repo"] != "o/r" {
			t.Fatalf("job request = %+v", job.Request)
		}
	}
	got := gh.posted()
	if len(got) != 4 {
		t.Fatalf("statuses = %+v, want a pending status per job", got)
	}
	for _, s := range got {
		if s.State != "pending" || s.Description != "Waiting for a VM" || !strings.HasPrefix(s.Context, "grove/") ||
			!strings.HasPrefix(s.TargetURL, "http://grove.example:6130/jobs/job") {
			t.Fatalf("status = %+v", s)
		}
	}

	// Nothing changed: nothing is submitted or posted again.
	w.Tick(ctx)
	if jobs.submits != 4 || len(gh.posted()) != 4 {
		t.Fatalf("an unchanged poll submitted %d / posted %d", jobs.submits, len(gh.posted()))
	}

	var testJob *dispatch.Job
	for _, job := range jobs.jobs {
		if job.Request.Ref == mainSHA && strings.HasSuffix(job.Request.IdempotencyKey, ":test") {
			testJob = job
		}
	}
	jobs.set(testJob.ID, dispatch.StatusFailed, 2)
	w.Tick(ctx)
	got = gh.posted()
	last := got[len(got)-1]
	if len(got) != 5 || last.sha != mainSHA || last.State != "failure" || last.Context != "grove/test" || last.Description != "Failed (exit 2) in 1m30s" {
		t.Fatalf("after the job failed: %+v", got)
	}
	// A terminal status is posted once.
	w.Tick(ctx)
	if len(gh.posted()) != 5 {
		t.Fatalf("terminal status posted again: %+v", gh.posted())
	}

	// A new push runs again.
	gh.mu.Lock()
	gh.branches["main"] = "4444444444444444444444444444444444444444"
	gh.mu.Unlock()
	w.Tick(ctx)
	if jobs.submits != 4 {
		t.Fatalf("a commit with no .grove.yml started jobs (submits %d)", jobs.submits)
	}
	if tok := gh.tokens[0]; tok != "tok" {
		t.Fatalf("GitHub called with token %q", tok)
	}
}

func TestWatcher_RestartDoesNotRerun(t *testing.T) {
	gh := &fakeGitHub{branches: map[string]string{"main": mainSHA}, files: map[string]string{mainSHA: goodSpec}}
	jobs := newFakeJobs()
	w := newWatcher(t, gh, jobs)
	w.Tick(context.Background())

	again := newWatcher(t, gh, jobs)
	again.StatePath = w.StatePath
	again.Tick(context.Background())
	if jobs.submits != 2 || len(gh.posted()) != 2 {
		t.Fatalf("a restarted watcher submitted %d / posted %d, want nothing new", jobs.submits, len(gh.posted()))
	}
}

func TestWatcher_BadSpecIsReportedOnTheCommit(t *testing.T) {
	gh := &fakeGitHub{branches: map[string]string{"main": mainSHA}, files: map[string]string{mainSHA: "jobs:\n  t:\n    script: x\n"}}
	jobs := newFakeJobs()
	w := newWatcher(t, gh, jobs)
	w.Tick(context.Background())
	got := gh.posted()
	if jobs.submits != 0 || len(got) != 1 || got[0].State != "error" || got[0].Context != "grove" || !strings.Contains(got[0].Description, "pool is required") {
		t.Fatalf("submits %d, statuses %+v", jobs.submits, got)
	}
}

func TestWatcher_SubmitFailureRetriesThenReports(t *testing.T) {
	gh := &fakeGitHub{branches: map[string]string{"main": mainSHA}, files: map[string]string{mainSHA: "jobs:\n  t:\n    pool: gpu\n    script: x\n"}}
	jobs := newFakeJobs()
	jobs.submitErr = errors.New(`dispatch: unknown pool "gpu"`)
	w := newWatcher(t, gh, jobs)
	for i := 0; i < maxStartAttempts-1; i++ {
		w.Tick(context.Background())
	}
	if len(gh.posted()) != 0 {
		t.Fatalf("posted before giving up: %+v", gh.posted())
	}
	w.Tick(context.Background())
	got := gh.posted()
	if len(got) != 1 || got[0].State != "error" || got[0].Description != `could not start: unknown pool "gpu"` {
		t.Fatalf("statuses %+v", got)
	}
}

func TestWatcher_FileReadErrorRetries(t *testing.T) {
	gh := &fakeGitHub{branches: map[string]string{"main": mainSHA}, files: map[string]string{mainSHA: goodSpec}, fileErr: errors.New("502")}
	jobs := newFakeJobs()
	w := newWatcher(t, gh, jobs)
	w.Tick(context.Background())
	gh.fileErr = nil
	w.Tick(context.Background())
	if jobs.submits != 2 {
		t.Fatalf("submits = %d after GitHub recovered", jobs.submits)
	}
}

func TestWatcher_SkipsRepoWithoutToken(t *testing.T) {
	gh := &fakeGitHub{branches: map[string]string{"main": mainSHA}, files: map[string]string{mainSHA: goodSpec}}
	jobs := newFakeJobs()
	w := newWatcher(t, gh, jobs)
	w.Auth = fakeAuth{}
	w.Tick(context.Background())
	if jobs.submits != 0 || len(gh.tokens) != 0 {
		t.Fatalf("a repository with no armed token was polled (%d calls) or built (%d)", len(gh.tokens), jobs.submits)
	}
}

func TestStatusDescriptions(t *testing.T) {
	jobs := newFakeJobs()
	w := newWatcher(t, &fakeGitHub{}, jobs)
	w.init()
	job, _, _ := jobs.Submit(context.Background(), dispatch.JobRequest{IdempotencyKey: "k:x"})
	reason := "the VM stopped answering"
	for _, tc := range []struct {
		status   dispatch.Status
		state    string
		desc     string
		terminal bool
	}{
		{dispatch.StatusRunning, "pending", "Running", false},
		{dispatch.StatusSuccess, "success", "Passed in 1m30s", true},
		{dispatch.StatusCanceled, "error", "Canceled", true},
		{dispatch.StatusLost, "error", "Lost: the VM stopped answering", true},
	} {
		jobs.set(job.ID, tc.status, 0)
		jobs.jobs[job.ID].FailureReason = &reason
		got, terminal, err := w.statusOf(context.Background(), &run{Context: "grove/x", JobID: job.ID})
		if err != nil || got.State != tc.state || got.Description != tc.desc || terminal != tc.terminal {
			t.Errorf("%s: %+v terminal=%v err=%v", tc.status, got, terminal, err)
		}
	}
	long := truncate(strings.Repeat("é", 200))
	if n := len([]rune(long)); n != maxDescription {
		t.Errorf("truncated to %d runes", n)
	}
}

func TestHTTPGitHub(t *testing.T) {
	var statusBody CommitStatus
	var mu sync.Mutex
	branchCalls, notModified := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/repos/o/r/branches/main":
			branchCalls++
			if r.Header.Get("If-None-Match") == `"v1"` {
				notModified++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			fmt.Fprintf(w, `{"commit":{"sha":%q}}`, mainSHA)
		case r.URL.Path == "/repos/o/r/pulls":
			fmt.Fprintf(w, `[{"number":7,"head":{"sha":%q,"repo":{"full_name":"o/r"}}},{"number":8,"head":{"sha":%q,"repo":null}}]`, prSHA, forkSHA)
		case r.URL.Path == "/repos/o/r/contents/.grove.yml" && r.URL.Query().Get("ref") == mainSHA:
			enc := base64.StdEncoding.EncodeToString([]byte(goodSpec))
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, enc[:10]+"\n"+enc[10:])
		case r.URL.Path == "/repos/o/r/statuses/"+mainSHA && r.Method == http.MethodPost:
			json.NewDecoder(r.Body).Decode(&statusBody)
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	g := NewHTTPGitHub()
	g.APIBase = srv.URL
	repo := Repo{Host: "github.com", FullName: "o/r"}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		sha, err := g.BranchHead(ctx, "tok", repo, "main")
		if err != nil || sha != mainSHA {
			t.Fatalf("BranchHead = %q, %v", sha, err)
		}
	}
	if branchCalls != 2 || notModified != 1 {
		t.Fatalf("branch calls %d, 304s %d: the repeat should be a conditional request", branchCalls, notModified)
	}
	prs, err := g.OpenPullRequests(ctx, "tok", repo)
	if err != nil || len(prs) != 2 || prs[0].HeadRepo != "o/r" || prs[1].HeadRepo != "" {
		t.Fatalf("prs = %+v, %v", prs, err)
	}
	data, err := g.File(ctx, "tok", repo, SpecPath, mainSHA)
	if err != nil || string(data) != goodSpec {
		t.Fatalf("File = %q, %v", data, err)
	}
	if _, err := g.File(ctx, "tok", repo, SpecPath, prSHA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file err = %v", err)
	}
	if err := g.SetStatus(ctx, "tok", repo, mainSHA, CommitStatus{State: "success", Context: "grove/test"}); err != nil {
		t.Fatal(err)
	}
	if statusBody.State != "success" || statusBody.Context != "grove/test" {
		t.Fatalf("posted %+v", statusBody)
	}
	_, err = g.BranchHead(ctx, "wrong", repo, "main")
	if err == nil || !strings.Contains(err.Error(), "401 Bad credentials") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("bad token err = %v", err)
	}
}

func TestRepoAPIBase(t *testing.T) {
	if got := (Repo{Host: "github.com"}).apiBase(); got != "https://api.github.com" {
		t.Errorf("github.com: %s", got)
	}
	if got := (Repo{Host: "ghe.example.com"}).apiBase(); got != "https://ghe.example.com/api/v3" {
		t.Errorf("enterprise: %s", got)
	}
	for _, bad := range []config.CIRepo{{Repo: "noslash"}, {Repo: "o/r/x"}, {Repo: "o/r", Host: "https://x"}} {
		if _, err := normalizeRepo(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
