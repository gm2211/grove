package cli

import (
	"context"
	"encoding/json"
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
	"github.com/gm2211/grove/internal/server"
	"github.com/gm2211/grove/internal/snooze"
	"github.com/gm2211/grove/internal/wire"
	"github.com/spf13/cobra"
)

func init() {
	Root.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Ping Orchard and Nomad (or the grove API) and print fleet counts.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, cfgPath, err := loadConfig()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()

		out := cmd.OutOrStdout()

		// A device that only has a server URL and token (a laptop, a worker Mac) asks the control
		// plane, which talks to Orchard and Nomad on its behalf.
		if statusViaAPI(cfg) {
			fl, ferr := fetchFleet(ctx)
			printPingResult(out, "server", cfg.Server.URL, ferr)
			if ferr != nil {
				return fmt.Errorf("the control plane check failed")
			}
			printFleetStatus(out, fl, time.Now())
			return nil
		}

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

// statusViaAPI says whether `grove status` should go through the grove API: the device has a server
// URL but not both Orchard and Nomad to ask directly.
func statusViaAPI(cfg *config.Config) bool {
	return cfg.Server.URL != "" && (cfg.Orchard.URL == "" || cfg.Nomad.URL == "")
}

// printFleetStatus prints, from GET /fleet, what the direct path prints from Orchard and Nomad.
func printFleetStatus(out io.Writer, fl *server.FleetResponse, now time.Time) {
	online := 0
	for _, w := range fl.Workers {
		if w.Online {
			online++
		}
	}
	fmt.Fprintf(out, "workers: %d (%d online)\n", len(fl.Workers), online)
	fmt.Fprintf(out, "vms: %d\n", len(fl.VMs))
	counts := map[string]int{}
	for _, n := range fl.Nodes {
		counts[n.Status]++
	}
	fmt.Fprintf(out, "nomad nodes: %s\n", statusCounts(len(fl.Nodes), counts))

	for _, w := range fl.Workers {
		if w.SnoozedUntil != nil && w.SnoozedUntil.After(now) {
			fmt.Fprintf(out, "snoozed: %s until %s\n", w.Name, w.SnoozedUntil.Local().Format("15:04"))
		}
	}

	vms := make([]orchard.VM, 0, len(fl.VMs))
	for _, e := range fl.VMs {
		var vm orchard.VM
		if b, err := json.Marshal(e.Raw); err == nil && json.Unmarshal(b, &vm) == nil {
			vms = append(vms, vm)
		}
	}
	for _, w := range fleet.BootLoops(vms) {
		fmt.Fprintf(out, "boot loop: %s\n", w.Describe())
	}
	for _, f := range fleet.LoginSessionFailures(vms) {
		fmt.Fprintf(out, "needs login: %s\n", f.Describe())
	}
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
	for _, f := range fleet.LoginSessionFailures(vms) {
		fmt.Fprintf(out, "needs login: %s\n", f.Describe())
	}
}

// nodeStatusCounts renders e.g. "3 (ready 2, down 1)", so stale down registrations don't read as
// capacity.
func nodeStatusCounts(nodes []nomad.Node) string {
	counts := map[string]int{}
	for _, n := range nodes {
		counts[n.Status]++
	}
	return statusCounts(len(nodes), counts)
}

func statusCounts(total int, counts map[string]int) string {
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
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
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
