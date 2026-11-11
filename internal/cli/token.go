package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/config"
)

var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage the control plane's operator token (server.token).",
}

var tokenRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Replace the operator token with a new random one and print it once.",
	Long: `Replace the control plane's operator token (server.token in config.yaml) with a new
256-bit random one. Run it on the control plane, as the user ` + "`grove serve`" + ` runs as.

The new token is written to config.yaml (atomically, mode 0600) and printed to stdout exactly
once; everything else goes to stderr, so ` + "`grove token rotate > token.txt`" + ` captures just the
token. The running ` + "`grove serve`" + ` keeps accepting the OLD token until it re-reads the
config: send it SIGHUP (it reloads only the operator token and keeps running jobs' credentials),
or restart it. The command prints how.

On the control plane nothing else holds a copy: the launchd/systemd units carry no token, and
every grove command there reads config.yaml when it starts. A long-running ` + "`grove mcp`" + ` that
uses the operator token must be restarted. Any other machine or script you gave the old token
(` + "`grove install --role client --token`" + `, ` + "`grove config set server.token`" + `, a CI
variable, the token box of the web UI) stops working and needs the new one.

Device credentials (` + "`grove access issue`" + `, ` + "`grove setup`" + `) and Tailscale-identity grants
are not affected. Prefer those day to day: grant ` + "`operator`" + ` to a tagged admin device in the
tailnet policy and keep this token for recovery (docs/OPERATIONS.md "Rotating the operator
token").`,
	Args: cobra.NoArgs,
	RunE: runTokenRotate,
}

func init() {
	tokenCmd.AddCommand(tokenRotateCmd)
	Root.AddCommand(tokenCmd)
}

func runTokenRotate(cmd *cobra.Command, _ []string) error {
	cfg, path, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Only the control plane's config.yaml is what `grove serve` authenticates against; anywhere
	// else this would just overwrite a copy and lock that machine out.
	if cfg.Server.Listen == "" {
		return errors.New("this machine is not the control plane (no server.listen in its config): run `grove token rotate` where `grove serve` runs, then give this machine the new token with `grove config set server.token -`")
	}
	token, err := newOperatorToken()
	if err != nil {
		return err
	}
	had := cfg.Server.Token != ""
	cfg.Server.Token = token
	if err := config.SaveAtomic(path, cfg); err != nil {
		return fmt.Errorf("write %s: %w (the operator token is unchanged)", path, err)
	}

	errOut := cmd.ErrOrStderr()
	if had {
		fmt.Fprintf(errOut, "New operator token written to %s. The old one still works until `grove serve` reloads.\n", path)
	} else {
		fmt.Fprintf(errOut, "Operator token written to %s (there was none before).\n", path)
	}
	fmt.Fprintln(errOut, "\nMake the running server take it up, either way:")
	if runtime.GOOS == "darwin" {
		fmt.Fprintln(errOut, "  launchctl kill SIGHUP gui/$(id -u)/com.grove.server       # reload the token only; jobs keep their credentials")
		fmt.Fprintln(errOut, "  launchctl kickstart -k gui/$(id -u)/com.grove.server      # or restart (running jobs' submitters must resend credentials)")
	} else {
		fmt.Fprintln(errOut, "  systemctl --user kill -s HUP grove-server.service         # reload the token only; jobs keep their credentials")
		fmt.Fprintln(errOut, "  systemctl --user restart grove-server.service             # or restart (running jobs' submitters must resend credentials)")
	}
	fmt.Fprintln(errOut, "A `grove serve` older than this command has no reload: restart it.")
	fmt.Fprintln(errOut, "Then check: `grove access list` here answers with the new token.")
	fmt.Fprintln(errOut, "\nNothing else on this machine holds a copy (restart a long-running `grove mcp` that uses it).")
	fmt.Fprintln(errOut, "Every other machine or script that had the old token needs this one: `grove config set server.token -` there.")
	fmt.Fprintln(errOut, "\nThe token, shown once:")
	fmt.Fprintln(cmd.OutOrStdout(), token)
	return nil
}

// newOperatorToken is 256 random bits, hex: the shape `grove install --role control-plane`
// generates the first one in.
func newOperatorToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate operator token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
