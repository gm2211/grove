package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Clone exit codes, the same ones the regular templates' source task exits with. A failed checkout
// (1) is reported by the job's run.sh, which does the checkout.
const (
	cloneRefused = 2
	cloneFailed  = 128
)

// cloneToken picks the clone credential: the control plane's armed token from the payload, else a
// GH_TOKEN/GIT_TOKEN the caller put in its own env (the regular templates' precedence).
func cloneToken(req Request) string {
	if req.CloneToken != "" {
		return req.CloneToken
	}
	for _, key := range cloneCredentialEnv {
		if v := req.Env[key]; v != "" {
			return v
		}
	}
	return ""
}

// Clone fetches repo into dir on the control plane, the way the regular templates' source task
// does inside the job (https only, a shallow clone), but without checking out any file: dir ends
// up holding only .git. The job's run.sh checks out ref inside its container, so no file of the
// repository is ever written on the control plane, where a hostile one (a symlink pointing out of
// the checkout, names that collide on a case-insensitive Mac disk) could do harm or come out
// wrong. ref is only vetted here. It returns 0 on success, otherwise the code the source task
// would exit with: 2 for a refused repo or ref, 128 when the clone fails. Git's output, and why a
// request was refused, go to log.
//
// The token reaches git only through its environment and an inline credential helper, never its
// argv or a config file, so the .git pushed into the VM carries no credential. The clone runs on
// the control plane as grove's own user, so it also switches off what that user's git config could
// add: other credential helpers (the login keychain) and hooks.
func Clone(ctx context.Context, git, repo, ref, token, dir string, log io.Writer) int {
	if !strings.HasPrefix(repo, "https://") {
		fmt.Fprintln(log, "grove: repo must be an https:// URL")
		return cloneRefused
	}
	if strings.HasPrefix(ref, "-") {
		fmt.Fprintln(log, "grove: ref must not start with '-'")
		return cloneRefused
	}

	env := append(os.Environ(), "GROVE_CLONE_TOKEN="+token, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	args := []string{"-c", "credential.helper=", "-c", "core.hooksPath=/dev/null"}
	if token != "" {
		const helper = `!f() { test "$1" = get || exit 0; echo username=x-access-token; printf "password=%s\n" "$GROVE_CLONE_TOKEN"; }; f`
		args = append(args, "-c", "credential.helper="+helper)
	}

	cmd := exec.CommandContext(ctx, git, append(args, "clone", "--no-checkout", "--depth", "50", "--", repo, dir)...)
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return cloneFailed
	}
	fetchRef(ctx, git, args, env, dir, ref, log)
	return 0
}

// fullSHA is a whole commit id, which a server can be asked for directly.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// fetchRef makes ref checkout-able in the clone at dir when the clone didn't bring it: a shallow
// clone has only the default branch, so a pull request's commit or another branch needs one more
// fetch. A commit id is fetched as itself; any other ref as the branch of that name, into a local
// branch, since run.sh's `git checkout <ref> --` doesn't guess one from a remote-tracking ref. A ref the clone already has costs no
// fetch, and one that can't be fetched is left for the checkout in the VM to report.
func fetchRef(ctx context.Context, git string, args, env []string, dir, ref string, log io.Writer) {
	if ref == "" || ref == "HEAD" {
		return
	}
	verify := exec.CommandContext(ctx, git, "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	verify.Env = withoutCloneToken(env)
	if verify.Run() == nil {
		return
	}
	spec := ref
	if !fullSHA.MatchString(ref) {
		spec = "+refs/heads/" + ref + ":refs/heads/" + ref
	}
	cmd := exec.CommandContext(ctx, git, append(args, "-C", dir, "fetch", "--quiet", "--depth", "50", "origin", spec)...)
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(log, "grove: could not fetch %s\n", ref)
	}
}

// withoutCloneToken is env with the clone token blanked, for git commands that don't talk to the
// server.
func withoutCloneToken(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GROVE_CLONE_TOKEN=") {
			kv = "GROVE_CLONE_TOKEN="
		}
		out = append(out, kv)
	}
	return out
}

// gitBinary finds git for Clone. Nomad's raw_exec tasks inherit the control plane agent's PATH,
// which under launchd is only the system directories, so fall back to /usr/bin/git.
func gitBinary() string {
	if path, err := exec.LookPath("git"); err == nil {
		return path
	}
	return "/usr/bin/git"
}
