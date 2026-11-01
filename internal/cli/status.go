package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/snooze"
	"github.com/gm2211/grove/internal/wire"
	"github.com/spf13/cobra"
)

func init() {
	Root.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Ping Orchard and Nomad and print fleet counts.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, cfgPath, err := loadConfig()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()

		out := cmd.OutOrStdout()

		orchardClient, orchardErr := newOrchardClient(cfg)
		if orchardErr == nil {
			orchardErr = orchardClient.Ping(ctx)
		}
		printPingResult(out, "orchard", cfg.Orchard.URL, orchardErr)

		nomadClient, nomadErr := newNomadClient(cfg)
		if nomadErr == nil {
			nomadErr = nomadClient.Ping(ctx)
		}
		printPingResult(out, "nomad", cfg.Nomad.URL, nomadErr)

		if orchardErr == nil {
			printOrchardCounts(ctx, out, orchardClient)
			printSnoozed(out, cfgPath)
		}

		if nomadErr == nil {
			printNomadCounts(ctx, out, nomadClient)
			printNomadPermissions(ctx, out, nomadClient)
		}
		if orchardErr == nil {
			printBootLoops(ctx, out, orchardClient)
		}

		if orchardErr != nil || nomadErr != nil {
			return fmt.Errorf("one or more control-plane checks failed")
		}

		return nil
	},
}

// printSnoozed lists the workers snoozed right now, from the control plane's pause store (only
// present on the control plane).
func printSnoozed(out io.Writer, cfgPath string) {
	st, err := snooze.Load(filepath.Join(filepath.Dir(cfgPath), "pauses.json"))
	if err != nil {
		return
	}
	now := time.Now()
	entries := st.List()
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if e := entries[n]; e.Active(now) {
			fmt.Fprintf(out, "snoozed: %s until %s\n", n, e.Snooze.Until.Local().Format("15:04"))
		}
	}
}

func printOrchardCounts(ctx context.Context, out io.Writer, client orchard.Client) {
	workers, err := client.ListWorkers(ctx)
	if err != nil {
		fmt.Fprintf(out, "workers: error: %v\n", err)
		return
	}

	online := 0
	for _, w := range workers {
		if !w.Offline {
			online++
		}
	}
	fmt.Fprintf(out, "workers: %d (%d online)\n", len(workers), online)

	vms, err := client.ListVMs(ctx)
	if err != nil {
		fmt.Fprintf(out, "vms: error: %v\n", err)
		return
	}
	fmt.Fprintf(out, "vms: %d\n", len(vms))
}

func printNomadCounts(ctx context.Context, out io.Writer, client nomad.Client) {
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		fmt.Fprintf(out, "nomad nodes: error: %v\n", err)
		return
	}
	fmt.Fprintf(out, "nomad nodes: %s\n", nodeStatusCounts(nodes))
}

// printNomadPermissions reports a control-plane call Nomad refuses (a 403): "nomad: up" says
// nothing about whether the token can enrol VMs, and without that no VM ever becomes a node.
func printNomadPermissions(ctx context.Context, out io.Writer, client nomad.Client) {
	err := nomad.CheckManagement(ctx, client)
	switch {
	case err == nil:
	case nomad.IsPermissionDenied(err):
		fmt.Fprintf(out, "nomad token: DENIED: %s (%v)\n", nomad.ManagementTokenHint, err)
	default:
		fmt.Fprintf(out, "nomad token: error: %v\n", err)
	}
}

// printBootLoops lists, per worker, the VMs Orchard keeps restarting.
func printBootLoops(ctx context.Context, out io.Writer, client orchard.Client) {
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return // printOrchardCounts already reported it
	}
	for _, w := range fleet.BootLoops(vms) {
		fmt.Fprintf(out, "boot loop: %s\n", w.Describe())
	}
}

// nodeStatusCounts renders e.g. "3 (ready 2, down 1)", so stale down registrations don't read as
// capacity.
func nodeStatusCounts(nodes []nomad.Node) string {
	counts := map[string]int{}
	for _, n := range nodes {
		counts[n.Status]++
	}
	statuses := make([]string, 0, len(counts))
	for s := range counts {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	parts := make([]string, len(statuses))
	for i, s := range statuses {
		parts[i] = fmt.Sprintf("%s %d", s, counts[s])
	}
	if len(parts) == 0 {
		return "0"
	}
	return fmt.Sprintf("%d (%s)", len(nodes), strings.Join(parts, ", "))
}

func printPingResult(w io.Writer, name, url string, err error) {
	if err != nil {
		fmt.Fprintf(w, "%-8s %-30s FAIL: %v\n", name, url, err)
		return
	}
	fmt.Fprintf(w, "%-8s %-30s OK\n", name, url)
}

// loadConfig loads grove's config, surfacing config.ErrNotFound's friendly message unchanged.
func loadConfig() (*config.Config, string, error) {
	return config.Load()
}

func newOrchardClient(cfg *config.Config) (orchard.Client, error) {
	if cfg.Orchard.URL == "" {
		return nil, fmt.Errorf("orchard.url is not configured; run `grove config set orchard.url <url>`")
	}

	return orchard.New(cfg.Orchard.URL, cfg.Orchard.Token)
}

func newNomadClient(cfg *config.Config) (nomad.Client, error) {
	if cfg.Nomad.URL == "" {
		return nil, fmt.Errorf("nomad.url is not configured; run `grove config set nomad.url <url>`")
	}

	return wire.NewNomadClient(cfg.Nomad)
}
