package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

var selfUpdateCheck bool

var selfUpdateCmd = &cobra.Command{
	Use:   "self-update",
	Short: "Install the latest grove release over this binary (checksum-verified)",
	Long: `Downloads the latest grove release, verifies it against the release's checksums.txt, and
replaces this binary, keeping the old one beside it as grove.<version>.bak. If a worker is
installed for this user, its install is re-run with the new binary (no sudo).

` + "`grove serve`" + ` and the worker's broker do this by themselves, hourly, while no job is
running, and then restart onto the new binary. Set ` + "`update: {auto: false}`" + ` in config.yaml to turn
that off. A Homebrew-installed grove is upgraded with ` + "`brew upgrade grove`" + ` instead.`,
	Args: cobra.NoArgs,
	RunE: runSelfUpdate,
}

func init() {
	selfUpdateCmd.Flags().BoolVar(&selfUpdateCheck, "check", false, "only say whether a newer release exists")
	Root.AddCommand(selfUpdateCmd)
}

func runSelfUpdate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	u, err := newUpdater()
	if err != nil {
		return err
	}
	latest, err := install.LatestGroveRelease(ctx, u.runner)
	if err != nil {
		return err
	}
	onDisk := u.diskVersion(ctx)
	if !install.NewerVersion(latest, onDisk) {
		fmt.Fprintf(out, "grove %s is the latest release\n", onDisk)
		return u.finishPending(ctx, out)
	}
	if selfUpdateCheck {
		fmt.Fprintf(out, "grove %s is available (this is %s)\n", strings.TrimPrefix(latest, "v"), onDisk)
		return nil
	}
	if err := u.update(ctx, latest, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "grove's running daemons restart onto it by themselves within minutes, once no job is running")
	return nil
}

// updater installs newer grove releases over the binary this process runs from.
type updater struct {
	runner install.Runner
	opts   install.Options
	exe    string // this binary, symlinks resolved
	dir    string // the grove config directory, for the lock and the pending marker
}

func newUpdater() (*updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	path, err := config.DefaultPath()
	if err != nil {
		return nil, err
	}
	return &updater{runner: install.ExecRunner{}, opts: install.Options{GroveVersion: Version}, exe: exe, dir: filepath.Dir(path)}, nil
}

// diskVersion is the version of the binary on disk, which is newer than this process once an
// update was installed under it.
func (u *updater) diskVersion(ctx context.Context) string {
	if v, err := install.InstalledGroveVersion(ctx, u.runner, u.exe); err == nil {
		return v
	}
	return Version
}

func (u *updater) pendingPath() string { return filepath.Join(u.dir, "update-pending") }

// update installs release latest and re-runs the worker install. One updater at a time, across
// processes: `grove serve` and the broker both run one on a control-plane Mac.
func (u *updater) update(ctx context.Context, latest string, out io.Writer) error {
	unlock, err := lockFile(filepath.Join(u.dir, "update.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if !install.NewerVersion(latest, u.diskVersion(ctx)) {
		return nil // another process installed it while this one waited
	}
	// Written first: if this process dies between the new binary and the worker install (the
	// install restarts the broker, which may be this process), the next start finishes it.
	if err := os.WriteFile(u.pendingPath(), []byte(latest+"\n"), 0o600); err != nil {
		return err
	}
	if err := install.SelfUpdate(ctx, u.runner, u.opts, u.exe, latest, out); err != nil {
		os.Remove(u.pendingPath())
		return err
	}
	return u.finishPendingLocked(ctx, out)
}

// finishPending re-runs the worker install an earlier update did not get to.
func (u *updater) finishPending(ctx context.Context, out io.Writer) error {
	if _, err := os.Stat(u.pendingPath()); err != nil {
		return nil
	}
	unlock, err := lockFile(filepath.Join(u.dir, "update.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return u.finishPendingLocked(ctx, out)
}

func (u *updater) finishPendingLocked(ctx context.Context, out io.Writer) error {
	if _, err := os.Stat(u.pendingPath()); err != nil {
		return nil
	}
	if install.WorkerInstalledHere(u.opts) {
		// The new binary's own install: it rewrites what changed (the worker wrapper) and restarts
		// what it rewrote. --no-sudo: a root step that changed is printed, never run from here.
		ictx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		c := exec.CommandContext(ictx, u.exe, "install", "--role", "worker", "--no-sudo")
		var buf bytes.Buffer
		c.Stdout, c.Stderr = &buf, &buf
		if err := c.Run(); err != nil {
			return fmt.Errorf("re-run the worker install with the new grove: %w\n%s", err, tail(buf.String(), 2000))
		}
		fmt.Fprintln(out, "worker install re-run with the new grove")
	}
	return os.Remove(u.pendingPath())
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

const (
	autoUpdateFirstCheck = 2 * time.Minute
	autoUpdateTick       = 10 * time.Minute
	autoUpdateEvery      = 6 // ticks between look-ups of the latest release: hourly
)

// runAutoUpdate keeps a long-running grove daemon (name, for the log) on the latest release:
// hourly, while idle() says no job depends on this process, it installs a newer release; and
// whenever the binary on disk is newer than this process, it calls restart, which must end the
// process so its supervisor (launchd KeepAlive, systemd Restart=always) starts the new binary.
// It returns at once for a development build, an install it can't replace, or update.auto: false.
func runAutoUpdate(ctx context.Context, name string, idle func(context.Context) bool, restart func()) {
	u, err := newUpdater()
	if err != nil {
		return
	}
	if !install.NewerVersion("v999999.0.0", Version) {
		return // not a release build
	}
	if cfg, _, err := config.Load(); err == nil && !cfg.AutoUpdate() {
		slog.Info(name + ": self-update is off (update.auto: false)")
		return
	}
	if err := install.SelfUpdatable(u.exe); err != nil {
		slog.Info(name+": not updating grove by itself", "reason", err.Error())
		// Still restart onto a binary someone else (brew) upgraded underneath.
	}
	timer := time.NewTimer(autoUpdateFirstCheck)
	defer timer.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(autoUpdateTick)
		if !idle(ctx) {
			continue
		}
		if n%autoUpdateEvery == 0 && install.SelfUpdatable(u.exe) == nil {
			if err := u.check(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn(name+": self-update failed; trying again in an hour", "err", err)
			}
		}
		if disk := u.diskVersion(ctx); install.NewerVersion(disk, Version) {
			slog.Info(name+": restarting onto the grove now installed", "running", Version, "installed", disk)
			restart()
			return
		}
	}
}

func (u *updater) check(ctx context.Context) error {
	var log bytes.Buffer
	if err := u.finishPending(ctx, &log); err != nil {
		return err
	}
	latest, err := install.LatestGroveRelease(ctx, u.runner)
	if err != nil {
		return err
	}
	if !install.NewerVersion(latest, u.diskVersion(ctx)) {
		return nil
	}
	err = u.update(ctx, latest, &log)
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if line != "" {
			slog.Info("self-update: " + line)
		}
	}
	return err
}

// restartSelf ends this process the way its supervisor's stop would, so it shuts down cleanly and
// is started again on the new binary.
func restartSelf() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }
