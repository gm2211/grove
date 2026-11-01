package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/install"
	"github.com/gm2211/grove/internal/server"
)

func init() {
	var f snoozeFlags
	cmd := &cobra.Command{
		Use:   "snooze [<worker>]",
		Short: "Pause a worker for a while (default 2h, at most 24h); it resumes by itself.",
		Long: `Snooze a worker Mac so you can use it yourself. Nothing new is placed on it, its VMs
drain and are removed, and it comes back on its own when the snooze ends. Jobs wait in their
pool's queue for any other worker; they only wait for this one if it is the only worker that
could run them. Without a worker name, this Mac's worker is used.

By default running jobs finish first. With --now, build and shell jobs on the worker are
requeued on other workers at once; agent jobs are never interrupted and keep running.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runWorkerSnooze(cmd, args, f) },
	}
	cmd.Flags().StringVar(&f.forDur, "for", "", "how long to snooze, e.g. 90m or 2h (default 2h, max 24h)")
	cmd.Flags().StringVar(&f.until, "until", "", "snooze until a local clock time, e.g. 18:00 (a time already past means tomorrow)")
	cmd.Flags().BoolVar(&f.now, "now", false, "also requeue running build/shell jobs on other workers")
	cmd.Flags().StringVar(&f.note, "note", "", "a note shown next to the snooze")
	workerCmd.AddCommand(cmd)
}

type snoozeFlags struct {
	forDur, until, note string
	now                 bool
}

// buildSnoozeRequest validates the flags and turns them into an API request. --until is a local
// clock time; one that is not in the future means the same time tomorrow.
func buildSnoozeRequest(f snoozeFlags, now time.Time) (server.SnoozeRequest, error) {
	if f.forDur != "" && f.until != "" {
		return server.SnoozeRequest{}, errors.New("use --for or --until, not both")
	}
	req := server.SnoozeRequest{Now: f.now, Note: f.note}
	if f.forDur != "" {
		d, err := time.ParseDuration(f.forDur)
		if err != nil || d <= 0 {
			return req, fmt.Errorf("--for %q is not a positive duration like 90m or 2h", f.forDur)
		}
		req.For = d.String()
	}
	if f.until != "" {
		t, err := parseLocalClock(f.until, now)
		if err != nil {
			return req, err
		}
		req.Until = t.UTC().Format(time.RFC3339)
	}
	return req, nil
}

func parseLocalClock(s string, now time.Time) (time.Time, error) {
	clock, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("--until %q is not a clock time like 18:00", s)
	}
	local := now.Local()
	t := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, local.Location())
	if !t.After(local) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

func runWorkerSnooze(cmd *cobra.Command, args []string, f snoozeFlags) error {
	req, err := buildSnoozeRequest(f, time.Now())
	if err != nil {
		return err
	}
	client, err := newAPIClient()
	if err != nil {
		return err
	}
	worker, err := workerArgOrDefault(cmd.Context(), client, args)
	if err != nil {
		return err
	}
	res, err := client.SnoozeWorker(cmd.Context(), worker, req)
	if err != nil {
		return snoozeAPIError(err, worker)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "snoozed %s until %s (%s)\n", res.Worker, res.Until.Local().Format("15:04"), res.Mode)
	if res.Mode == "now" {
		fmt.Fprintf(out, "requeued %d job(s) elsewhere; %d left running (agent jobs are not interrupted)\n", len(res.Requeued), len(res.LeftRunning))
	}
	fmt.Fprintf(out, "end it early with: grove worker wake %s\n", worker)
	return nil
}

// snoozeAPIError adds the exact command that grants this credential the right to snooze when the
// server answered 403.
func snoozeAPIError(err error, worker string) error {
	if err == nil || !strings.Contains(err.Error(), "403") {
		return err
	}
	return fmt.Errorf("%w\n\nthis credential may not snooze %s. On the control plane run:\n  grove access issue --name %s-owner --scope read --scope worker:snooze:%s --expires-in 180d\nthen here:\n  grove config set server.token -   (paste the token)",
		err, worker, worker, worker)
}

// workerArgOrDefault is args[0], else this Mac's worker.
func workerArgOrDefault(ctx context.Context, c *apiclient.Client, args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	return defaultWorker(ctx, c, install.ReadWorkerName, os.Hostname)
}

type workerLookup interface {
	WhoAmI(ctx context.Context) (*server.Principal, error)
	Fleet(ctx context.Context) (*server.FleetResponse, error)
}

// defaultWorker finds which worker "this Mac" is: the installer's worker-identity.json, else the
// one worker:snooze:<name> scope this credential holds, else the host name matched against the
// fleet's workers by their Tailscale-style label.
func defaultWorker(ctx context.Context, c workerLookup, identity func() (string, bool), hostname func() (string, error)) (string, error) {
	if name, ok := identity(); ok && name != "" {
		return name, nil
	}
	if p, err := c.WhoAmI(ctx); err == nil {
		var names []string
		for _, s := range p.Scopes {
			if w, ok := strings.CutPrefix(s, server.ScopeSnoozePrefix); ok {
				names = append(names, w)
			}
		}
		if len(names) == 1 {
			return names[0], nil
		}
	}
	host, err := hostname()
	if err == nil {
		if fl, ferr := c.Fleet(ctx); ferr == nil {
			var matches []string
			for _, w := range fl.Workers {
				if hostLabel(w.Name) == hostLabel(host) {
					matches = append(matches, w.Name)
				}
			}
			sort.Strings(matches)
			if len(matches) == 1 {
				return matches[0], nil
			}
		}
	}
	return "", errors.New("could not tell which worker this Mac is; name it: grove worker snooze <worker>")
}

// hostLabel reduces a hostname or MagicDNS name to the label Tailscale would give it: the first
// DNS label, lowercased, with anything but letters, digits and '-' turned into '-'.
func hostLabel(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
