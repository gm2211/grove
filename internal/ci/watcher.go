package ci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/dispatch"
)

// forgetAfter is how long a commit is remembered once it was first seen. A commit older than this
// that comes back as a branch head (a branch reset to an old commit) runs again.
const forgetAfter = 14 * 24 * time.Hour

// maxStartAttempts is how many polls in a row may fail to submit a commit's jobs before the
// failure is posted on the commit.
const maxStartAttempts = 3

// maxDescription is GitHub's limit on a commit status description.
const maxDescription = 140

// Watcher polls GitHub for new commits on the configured repositories, starts their .grove.yml
// jobs, and posts each job's progress back as a commit status.
type Watcher struct {
	Repos  []config.CIRepo
	GitHub GitHub
	// Auth supplies the token for each repository: the operator-armed private-repo credential.
	// A repository it has no token for is skipped, since without one grove can't post results.
	Auth dispatch.RepoAuth
	Jobs dispatch.Service
	// ServerURL, when set, makes each status link to the job's page in grove's UI.
	ServerURL string
	// StatePath is where the watcher's memory lives; empty means ci.json next to jobs.json.
	StatePath string
	Log       *slog.Logger
	Now       func() time.Time

	state *state
	// failedStarts counts the polls on which a commit's jobs could not all be submitted. Grove
	// retries (idempotency keys make a resubmit safe) before telling GitHub it gave up.
	failedStarts map[string]int
	// warned holds the last warning logged per repository, so a problem that lasts is logged
	// once rather than every poll.
	warned map[string]string
}

// Run polls immediately and then every interval until ctx is done.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = config.DefaultCIInterval
	}
	if err := w.init(); err != nil {
		return err
	}
	w.Tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

func (w *Watcher) init() error {
	if w.state != nil {
		return nil
	}
	if w.StatePath == "" {
		w.StatePath = defaultStatePath()
	}
	st, err := loadState(w.StatePath)
	if err != nil {
		return fmt.Errorf("ci: load state: %w", err)
	}
	w.state = st
	if w.warned == nil {
		w.warned = map[string]string{}
	}
	if w.failedStarts == nil {
		w.failedStarts = map[string]int{}
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	return nil
}

// Tick does one poll: start jobs for every new commit, then report on every unfinished one.
func (w *Watcher) Tick(ctx context.Context) {
	if err := w.init(); err != nil {
		w.Log.Warn("ci: watcher disabled", "err", err)
		return
	}
	for _, rc := range w.Repos {
		repo, err := normalizeRepo(rc)
		if err != nil {
			w.warn(rc.Repo, "ci: skipping repository", "err", err)
			continue
		}
		token, ok := w.Auth.TokenFor(repo.CloneURL())
		if !ok {
			w.warn(repo.FullName, "ci: skipping repository: no armed GitHub token covers it (grove github enable --repo "+repo.FullName+")")
			continue
		}
		heads, err := w.heads(ctx, token, repo, rc)
		if err != nil {
			w.warn(repo.FullName, "ci: could not read repository from GitHub", "repo", repo.FullName, "err", err)
			continue
		}
		w.clearWarning(repo.FullName)
		for _, h := range heads {
			if _, seen := w.state.Commits[commitKey(repo, h.sha)]; seen {
				continue
			}
			w.start(ctx, token, repo, h)
		}
	}
	w.report(ctx)
	w.forget()
	if err := w.state.save(w.StatePath); err != nil {
		w.Log.Warn("ci: could not save state", "path", w.StatePath, "err", err)
	}
}

type head struct {
	sha     string
	trigger string // "push to main", "pull request #12"
}

// heads lists the commits CI should have run on: each configured branch's tip, then each open
// same-repository pull request's head. A commit that is both appears once, under its branch.
func (w *Watcher) heads(ctx context.Context, token string, repo Repo, rc config.CIRepo) ([]head, error) {
	var out []head
	seen := map[string]bool{}
	for _, branch := range rc.Branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		sha, err := w.GitHub.BranchHead(ctx, token, repo, branch)
		if err != nil {
			return nil, fmt.Errorf("branch %s: %w", branch, err)
		}
		if !seen[sha] {
			seen[sha] = true
			out = append(out, head{sha: sha, trigger: "push to " + branch})
		}
	}
	if rc.PullRequests {
		prs, err := w.GitHub.OpenPullRequests(ctx, token, repo)
		if err != nil {
			return nil, fmt.Errorf("pull requests: %w", err)
		}
		for _, pr := range prs {
			// A fork's .grove.yml is a stranger's script; it never runs on these machines.
			if !strings.EqualFold(pr.HeadRepo, repo.FullName) || pr.HeadSHA == "" || seen[pr.HeadSHA] {
				continue
			}
			seen[pr.HeadSHA] = true
			out = append(out, head{sha: pr.HeadSHA, trigger: fmt.Sprintf("pull request #%d", pr.Number)})
		}
	}
	return out, nil
}

// start reads the commit's .grove.yml and submits its jobs. A commit whose file can't be fetched
// right now is left unrecorded, so the next poll tries again.
func (w *Watcher) start(ctx context.Context, token string, repo Repo, h head) {
	c := &commit{Host: repo.Host, Repo: repo.FullName, SHA: h.sha, Trigger: h.trigger, SeenAt: w.Now().UTC()}
	data, err := w.GitHub.File(ctx, token, repo, SpecPath, h.sha)
	switch {
	case errors.Is(err, ErrNotFound):
		// No .grove.yml: this commit has no grove CI. Remember it so the file isn't fetched again.
		c.Finished = true
		w.state.Commits[commitKey(repo, h.sha)] = c
		return
	case err != nil:
		w.Log.Warn("ci: could not read "+SpecPath, "repo", repo.FullName, "sha", h.sha, "err", err)
		return
	}

	spec, err := ParseSpec(data)
	if err != nil {
		c.Runs = []*run{{Context: "grove", Error: err.Error()}}
		w.state.Commits[commitKey(repo, h.sha)] = c
		return
	}
	key := commitKey(repo, h.sha)
	meta := map[string]string{"ci.repo": repo.FullName, "ci.sha": h.sha, "ci.trigger": h.trigger}
	failed := false
	for _, name := range spec.JobNames() {
		r := &run{Context: "grove/" + name}
		job, _, err := w.Jobs.Submit(ctx, spec.Request(name, repo.CloneURL(), h.sha, meta))
		if err != nil {
			failed = true
			r.Error = "could not start: " + strings.TrimPrefix(err.Error(), "dispatch: ")
			w.Log.Warn("ci: could not start job", "repo", repo.FullName, "sha", h.sha, "job", name, "err", err)
		} else {
			r.JobID = job.ID
		}
		c.Runs = append(c.Runs, r)
	}
	// A refused submit may be passing (the control plane was briefly unable to dispatch), so the
	// commit is retried on the next polls; only then is the failure posted on the commit.
	if failed {
		w.failedStarts[key]++
		if w.failedStarts[key] < maxStartAttempts {
			return
		}
	}
	delete(w.failedStarts, key)
	w.state.Commits[key] = c
	w.Log.Info("ci: started jobs", "repo", repo.FullName, "sha", h.sha, "trigger", h.trigger, "jobs", len(c.Runs))
}

// report posts the current status of every unfinished run whose status changed since it was last
// posted. A failed post is retried on the next poll.
func (w *Watcher) report(ctx context.Context) {
	for _, c := range w.state.Commits {
		if c.Finished {
			continue
		}
		repo := c.repo()
		token, ok := w.Auth.TokenFor(repo.CloneURL())
		if !ok {
			continue
		}
		finished := true
		for _, r := range c.Runs {
			if r.Final {
				continue
			}
			status, terminal, err := w.statusOf(ctx, r)
			if err != nil {
				w.Log.Warn("ci: could not read job status", "repo", c.Repo, "sha", c.SHA, "job", r.JobID, "err", err)
				finished = false
				continue
			}
			if r.Posted == nil || *r.Posted != status {
				if err := w.GitHub.SetStatus(ctx, token, repo, c.SHA, status); err != nil {
					w.warn(c.Repo+" status", "ci: could not post commit status", "repo", c.Repo, "sha", c.SHA, "err", err)
					finished = false
					continue
				}
				w.clearWarning(c.Repo + " status")
				posted := status
				r.Posted = &posted
			}
			r.Final = terminal
			finished = finished && terminal
		}
		c.Finished = finished
	}
}

// statusOf is the commit status a run should show now, and whether it is the last one it will.
func (w *Watcher) statusOf(ctx context.Context, r *run) (CommitStatus, bool, error) {
	if r.JobID == "" {
		return CommitStatus{State: "error", Context: r.Context, Description: truncate(r.Error)}, true, nil
	}
	job, err := w.Jobs.Get(ctx, r.JobID)
	if errors.Is(err, dispatch.ErrNotFound) {
		return CommitStatus{State: "error", Context: r.Context, Description: "grove no longer has this job"}, true, nil
	}
	if err != nil {
		return CommitStatus{}, false, err
	}
	status := CommitStatus{Context: r.Context, TargetURL: w.jobURL(job.ID)}
	terminal := true
	switch job.Status {
	case dispatch.StatusPending:
		status.State, status.Description, terminal = "pending", "Waiting for a VM", false
	case dispatch.StatusRunning:
		status.State, status.Description, terminal = "pending", "Running", false
	case dispatch.StatusSuccess:
		status.State, status.Description = "success", "Passed"+took(job)
	case dispatch.StatusFailed:
		status.State, status.Description = "failure", "Failed"+exitCode(job)+took(job)
	case dispatch.StatusCanceled:
		status.State, status.Description = "error", "Canceled"
	case dispatch.StatusLost:
		status.State, status.Description = "error", "Lost"
		if job.FailureReason != nil && *job.FailureReason != "" {
			status.Description += ": " + *job.FailureReason
		}
	default:
		status.State, status.Description, terminal = "pending", string(job.Status), false
	}
	status.Description = truncate(status.Description)
	return status, terminal, nil
}

func (w *Watcher) jobURL(id string) string {
	if w.ServerURL == "" {
		return ""
	}
	return strings.TrimRight(w.ServerURL, "/") + "/jobs/" + id
}

// forget drops commits seen more than forgetAfter ago, so the state stays small.
func (w *Watcher) forget() {
	cutoff := w.Now().Add(-forgetAfter)
	for key, c := range w.state.Commits {
		if c.SeenAt.Before(cutoff) {
			delete(w.state.Commits, key)
		}
	}
}

func (w *Watcher) warn(key, msg string, args ...any) {
	line := fmt.Sprint(append([]any{msg}, args...)...)
	if w.warned[key] == line {
		return
	}
	w.warned[key] = line
	w.Log.Warn(msg, args...)
}

func (w *Watcher) clearWarning(key string) { delete(w.warned, key) }

// normalizeRepo validates a configured repository and fills in the default host.
func normalizeRepo(rc config.CIRepo) (Repo, error) {
	host := strings.ToLower(strings.TrimSpace(rc.Host))
	if host == "" {
		host = "github.com"
	}
	if strings.ContainsAny(host, "/:@ ") {
		return Repo{}, fmt.Errorf("host %q is not a bare host name", rc.Host)
	}
	name := strings.TrimSuffix(strings.TrimSpace(rc.Repo), ".git")
	owner, repo, ok := strings.Cut(name, "/")
	if !ok || owner == "" || repo == "" || strings.ContainsAny(repo, "/ ") || strings.ContainsAny(owner, " ") {
		return Repo{}, fmt.Errorf("repo %q is not owner/name", rc.Repo)
	}
	return Repo{Host: host, FullName: name}, nil
}

func took(job *dispatch.Job) string {
	if job.StartedAt == nil || job.FinishedAt == nil {
		return ""
	}
	return " in " + job.FinishedAt.Sub(*job.StartedAt).Round(time.Second).String()
}

func exitCode(job *dispatch.Job) string {
	if job.TimedOut {
		return " (timed out)"
	}
	if job.ExitCode == nil {
		return ""
	}
	return fmt.Sprintf(" (exit %d)", *job.ExitCode)
}

func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxDescription {
		return string(r[:maxDescription-1]) + "…"
	}
	return s
}
