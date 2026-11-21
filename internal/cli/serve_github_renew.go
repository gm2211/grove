package cli

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/gitauth"
	"github.com/gm2211/grove/internal/install"
)

// githubRenewInterval is how often `grove serve` looks whether the armed clone credential is due
// for a fresh token. Due means half its lifetime is gone, so a failing source is retried hourly
// for the other half before the credential lapses.
const githubRenewInterval = time.Hour

// ghToken returns what `gh auth token` prints for the user this process runs as. The token goes
// from gh's stdout into memory and nowhere else: never argv, the environment, a file or a log.
// gh is looked up with Homebrew's directories on PATH, which a launchd job doesn't have.
var ghToken = func(ctx context.Context) (string, error) {
	home, _ := os.UserHomeDir()
	path := install.UnattendedPath(os.Getenv("PATH"), home)
	gh := "gh"
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if info, err := os.Stat(dir + "/gh"); err == nil && !info.IsDir() {
			gh = dir + "/gh"
			break
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "auth", "token")
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// renewGitHubOnce swaps a fresh token into store when its credential is armed to renew itself
// and due. It reports whether it renewed.
func renewGitHubOnce(ctx context.Context, store *gitauth.Store, now time.Time) (bool, error) {
	if !store.RenewDue(now) {
		return false, nil
	}
	token, err := ghToken(ctx)
	if err != nil {
		return false, err
	}
	if _, err := store.Renew(token, now); err != nil {
		return false, err
	}
	return true, nil
}

// runGitHubRenewal keeps a self-renewing clone credential alive until ctx is done.
func runGitHubRenewal(ctx context.Context, store *gitauth.Store) {
	var warned repeatedWarning
	tick := func() {
		renewed, err := renewGitHubOnce(ctx, store, time.Now())
		switch {
		case err != nil:
			if warned.due(err.Error(), time.Now()) {
				slog.Warn("serve: could not renew the private-repo credential from gh; it lapses at its expiry unless this recovers", "err", err)
			}
		case renewed:
			warned.clear()
			st := store.Status()
			slog.Info("serve: renewed the private-repo credential from gh", "fingerprint", st.TokenFingerprint, "expiresAt", st.ExpiresAt)
		}
	}
	tick()
	t := time.NewTicker(githubRenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
