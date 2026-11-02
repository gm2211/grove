package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/gm2211/grove/internal/server"
	"github.com/spf13/cobra"
)

func init() {
	Root.AddCommand(workerCmd)
	workerCmd.AddCommand(workerLsCmd)
	workerCmd.AddCommand(workerPauseCmd)
	workerCmd.AddCommand(workerResumeCmd)
}

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Inspect and control Orchard workers.",
}

var workerLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List workers.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tARCH\tRUNTIME\tONLINE\tPAUSED\tSNOOZED\tLAST SEEN")

		// The control plane's /fleet knows about snoozes; Orchard alone does not. A device with a
		// server URL goes through the API, and only falls back to Orchard (with a warning) when it
		// also has Orchard configured; otherwise the API's error is the answer.
		if cfg.Server.URL != "" {
			fl, ferr := fetchFleet(cmd.Context())
			if ferr == nil {
				writeWorkerRows(w, fl.Workers, time.Now())
				return w.Flush()
			}
			if cfg.Orchard.URL == "" {
				return fmt.Errorf("list workers through %s: %w", cfg.Server.URL, ferr)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %v; asking Orchard directly (no snooze information)\n", cfg.Server.URL, ferr)
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}
		workers, err := client.ListWorkers(cmd.Context())
		if err != nil {
			return err
		}
		sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })
		for _, wk := range workers {
			lastSeen := "-"
			if !wk.LastSeen.IsZero() {
				lastSeen = time.Since(wk.LastSeen).Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\t%s\t%s\n", wk.Name, wk.Arch, wk.Runtime, !wk.Offline, wk.SchedulingPaused, "?", lastSeen)
		}
		return w.Flush()
	},
}

// fetchFleet reads GET /fleet with this device's server URL and token.
func fetchFleet(ctx context.Context) (*server.FleetResponse, error) {
	api, err := newAPIClient()
	if err != nil {
		return nil, err
	}
	return api.Fleet(ctx)
}

// writeWorkerRows renders /fleet's worker entries as `grove worker ls` rows.
func writeWorkerRows(w io.Writer, workers []server.FleetEntry, now time.Time) {
	for _, e := range workers {
		var raw struct {
			Runtime  string    `json:"runtime"`
			LastSeen time.Time `json:"lastSeen"`
		}
		if b, err := json.Marshal(e.Raw); err == nil {
			_ = json.Unmarshal(b, &raw)
		}
		lastSeen := "-"
		if !raw.LastSeen.IsZero() {
			lastSeen = now.Sub(raw.LastSeen).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\t%s\t%s\n", e.Name, e.Arch, raw.Runtime, e.Online, e.Cordoned, snoozedColumn(e.SnoozedUntil), lastSeen)
	}
}

func snoozedColumn(until *time.Time) string {
	if until == nil {
		return "-"
	}
	return "until " + until.Local().Format("15:04")
}

var workerPauseCmd = &cobra.Command{
	Use:   "pause <worker>",
	Short: "Pause a worker: no new work, and its VMs drain and are removed (grove worker resume brings it back).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := pauseOrResume(cmd, args[0], true); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "paused %s\n", args[0])
		return nil
	},
}

var workerResumeCmd = &cobra.Command{
	Use:   "resume <worker>",
	Short: "Resume scheduling on a worker (a snooze is ended with `grove worker wake`).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := pauseOrResume(cmd, args[0], false); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "resumed %s\n", args[0])
		return nil
	},
}

// pauseOrResume goes through the control plane's API when this device has a server URL and token,
// so the reason is recorded (a snooze ending later must not undo it). Otherwise it talks to
// Orchard directly and warns that the pause leaves no reason on record.
func pauseOrResume(cmd *cobra.Command, worker string, pause bool) error {
	if api, err := newAPIClient(); err == nil {
		if pause {
			return api.PauseWorker(cmd.Context(), worker)
		}
		return api.ResumeWorker(cmd.Context(), worker)
	}
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	client, err := newOrchardClient(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "warning: no server.url/server.token configured; talking to Orchard directly, so the control plane does not record why the worker is paused")
	if pause {
		return client.PauseWorker(cmd.Context(), worker)
	}
	return client.ResumeWorker(cmd.Context(), worker)
}
