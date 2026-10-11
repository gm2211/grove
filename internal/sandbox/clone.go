package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	return 0
}

// gitBinary finds git for Clone. Nomad's raw_exec tasks inherit the control plane agent's PATH,
// which under launchd is only the system directories, so fall back to /usr/bin/git.
func gitBinary() string {
	if path, err := exec.LookPath("git"); err == nil {
		return path
	}
	return "/usr/bin/git"
}
