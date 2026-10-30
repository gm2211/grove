package cli

// `grove setup --user <name>`: an admin sets this Mac up as a worker that runs as a standard user,
// with one sudo password prompt and no `su`. The admin's grove does the join itself, then runs the
// worker user's half (`grove setup --from-admin`, the join result on stdin) through `sudo -u`, and
// then renders the root script itself and runs it through the same sudo session. The worker user
// is not trusted with root, so nothing that user's process prints, writes or stages ever becomes
// root script text: every line comes from this process's rendering of the user's name and home,
// the grove binary, and the controller and server the join returned, and every plist the user
// staged is copied as root's and checked against the SHA-256 of the plist this process renders.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

// workerTarget is the standard user an admin's `grove setup --user` sets the worker up for.
type workerTarget struct {
	User     string
	Home     string // from the directory service, the same HOME `sudo -H` gives the user's half
	GroveBin string // the grove binary both halves name, so both render the same broker plist
}

func (t *workerTarget) options() install.Options {
	return install.Options{User: t.User, Home: t.Home, GroveBin: t.GroveBin}
}

// lookupWorkerTarget checks that this account can set a worker up for name, and resolves it.
func lookupWorkerTarget(ctx context.Context, r install.Runner, name string) (*workerTarget, error) {
	if name == "root" {
		return nil, errors.New("--user must name an ordinary account: the worker never runs as root")
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("--user %s: %w", name, err)
	}
	if u.Uid == "0" {
		return nil, fmt.Errorf("--user %s is uid 0: the worker never runs as root", name)
	}
	if me, err := user.Current(); err == nil && me.Username == u.Username {
		return nil, fmt.Errorf("--user %s is this account: run `grove setup` without --user", name)
	}
	if u.HomeDir == "" || !filepath.IsAbs(u.HomeDir) {
		return nil, fmt.Errorf("--user %s has no home directory", name)
	}
	if os.Getenv("GROVE_CONFIG") != "" {
		// It would point this process's idea of the user's config dir somewhere else than the
		// user's half (which sudo starts with a clean environment) stages into.
		return nil, errors.New("unset GROVE_CONFIG to run `grove setup --user`: it names this account's config, not the worker user's")
	}
	if os.Geteuid() != 0 {
		stdout, _, err := r.Run(ctx, "/usr/bin/id", "-Gn")
		if err != nil || !hasField(stdout, "admin") {
			return nil, errors.New("`grove setup --user` needs an admin account (it runs the root steps through sudo); run it from one, or run `grove setup` as the worker user and have an admin run the block it prints")
		}
	}
	bin, err := exec.LookPath("grove")
	if err != nil {
		bin, err = os.Executable()
	}
	if err == nil {
		bin, err = filepath.Abs(bin)
	}
	if err != nil {
		return nil, fmt.Errorf("find the grove binary for %s to run: %w", name, err)
	}
	return &workerTarget{User: u.Username, Home: u.HomeDir, GroveBin: bin}, nil
}

func hasField(s, want string) bool {
	for _, f := range strings.Fields(s) {
		if f == want {
			return true
		}
	}
	return false
}

// sudoKeepAlive is how often a sudo session refreshes its timestamp. sudo's default timeout is 5
// minutes and the join approval can take 10.
var sudoKeepAlive = time.Minute

// startSudoSession asks for the admin's password once (`sudo -v`; with --yes and no terminal,
// cached credentials must already be there) and keeps the timestamp fresh until stop is called,
// so the user's half and the root script later run through `sudo -n` with no second prompt.
func startSudoSession(ctx context.Context, r install.Runner, interactive, yes bool) (stop func(), err error) {
	switch {
	case interactive:
		fmt.Fprintln(os.Stderr, "grove setup --user runs the worker user's half and then the root steps through sudo; sudo asks for this account's password once:")
		err = r.RunAttached(ctx, "/usr/bin/sudo", "-v")
	case yes:
		_, _, err = r.Run(ctx, "/usr/bin/sudo", "-n", "-v")
	default:
		return nil, errors.New("`grove setup --user` needs a terminal for the sudo password (or --yes with sudo credentials already cached)")
	}
	if err != nil {
		return nil, fmt.Errorf("sudo: %w", err)
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(sudoKeepAlive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_, _, _ = r.Run(ctx, "/usr/bin/sudo", "-n", "-v")
			}
		}
	}()
	return func() { close(done); <-exited }, nil
}

// adminHandoff is what the admin's grove sends the user's half on stdin: the join result and the
// registry token. Every field but the URLs is a secret, so it never goes on argv.
type adminHandoff struct {
	ControllerURL  string `json:"controllerUrl"`
	ServerURL      string `json:"serverUrl"`
	BootstrapToken string `json:"bootstrapToken"`
	BrokerToken    string `json:"brokerToken,omitempty"`
	RegistryToken  string `json:"registryToken,omitempty"`
	ClientToken    string `json:"clientToken,omitempty"`
	DeviceID       string `json:"deviceId,omitempty"`
}

// setupForUser runs the admin's side once the join is approved: the worker user's half through
// `sudo -n -u <user>` (its unprivileged steps; it stages the daemon plists), then the root steps
// this process renders for that user, through RunRootSteps and the same sudo session. base names
// the user, home and grove binary.
func setupForUser(ctx context.Context, r install.Runner, base install.Options, result *joinResult, registryUser, registryToken string, ro install.RootOptions) (install.RootOutcome, error) {
	if base.User == "" || base.User == "root" || base.GroveBin == "" {
		return install.RootNone, errors.New("setup --user: no worker user or grove binary")
	}
	payload, err := json.Marshal(adminHandoff{
		ControllerURL: result.ControllerURL, ServerURL: result.ServerURL,
		BootstrapToken: result.BootstrapToken, BrokerToken: result.BrokerToken, RegistryToken: registryToken,
		ClientToken: result.ClientToken, DeviceID: result.DeviceID,
	})
	if err != nil {
		return install.RootNone, err
	}
	args := []string{"-n", "-u", base.User, "-H", "--", base.GroveBin, "setup", "--from-admin", "--grove-bin", base.GroveBin}
	if registryUser != "" {
		args = append(args, "--registry-user", registryUser)
	}
	fmt.Fprintf(ro.Out, "\nInstalling the worker as %s:\n\n", base.User)
	if err := r.RunStreaming(ctx, string(payload), "/usr/bin/sudo", args...); err != nil {
		return install.RootNone, fmt.Errorf("the worker install as %s failed (see above): %w", base.User, err)
	}

	opts := base
	opts.Role = install.RoleWorker
	opts.Controller = result.ControllerURL
	opts.ServerURL = result.ServerURL
	steps, err := install.PendingRootSteps(ctx, r, opts)
	if err != nil {
		return install.RootNone, fmt.Errorf("render the root steps for %s: %w", base.User, err)
	}
	return install.RunRootSteps(ctx, r, steps, ro)
}

// runSetupFromAdmin is the worker user's half of `grove setup --user`, run by the admin's grove
// through `sudo -u`: the worker plan's unprivileged steps with the join result from stdin. The
// root steps are left to the admin's grove, which renders them itself; nothing this process prints
// is run as root.
func runSetupFromAdmin(cmd *cobra.Command) error {
	if os.Geteuid() == 0 {
		return errors.New("--from-admin runs as the worker user, never root")
	}
	var h adminHandoff
	if err := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 64<<10)).Decode(&h); err != nil {
		return fmt.Errorf("--from-admin reads the join result from stdin: %w", err)
	}
	if h.ControllerURL == "" || h.BootstrapToken == "" {
		return errors.New("--from-admin: the join result on stdin has no worker credentials")
	}
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Chdir(home) // sudo leaves the admin's working directory, which this user may not read
	}
	opts := install.Options{
		Role: install.RoleWorker, Controller: h.ControllerURL, ServerURL: h.ServerURL,
		Token: h.BootstrapToken, BrokerToken: h.BrokerToken,
		RegistryUser: setupRegistryUser, RegistryToken: h.RegistryToken, GroveBin: setupGroveBin,
	}
	if _, _, err := runInstallPlan(cmd, install.ExecRunner{}, opts, false, false); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "The [root] steps are left to the admin's grove, which renders and checks them itself.")
	if h.ClientToken != "" && h.DeviceID != "" {
		return saveJoinedServer(cmd.Context(), install.ExecRunner{}, cmd.ErrOrStderr(), h.ServerURL, h.DeviceID, h.ClientToken, false)
	}
	return nil
}
