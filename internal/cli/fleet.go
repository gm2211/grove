package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/spf13/cobra"
)

func init() {
	Root.AddCommand(fleetCmd)
	fleetCmd.AddCommand(fleetPlanCmd)
	fleetCmd.AddCommand(fleetApplyCmd)
	fleetCmd.AddCommand(fleetWatchCmd)
	fleetCmd.AddCommand(fleetStatusCmd)

	fleetWatchCmd.Flags().Duration("interval", 30*time.Second, "reconcile interval")
}

var fleetCmd = &cobra.Command{
	Use:   "fleet",
	Short: "Manage the desired-state fleet of worker VMs (fleet.yaml).",
}

var fleetPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show what would change to match fleet.yaml.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		r, err := newReconciler(cfg)
		if err != nil {
			return err
		}

		plan, err := r.Plan(cmd.Context())
		if err != nil {
			return err
		}

		printPlan(cmd.OutOrStdout(), plan)

		return nil
	},
}

var fleetApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Reconcile Orchard to match fleet.yaml once.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		r, err := newReconciler(cfg)
		if err != nil {
			return err
		}

		ctx := cmd.Context()

		plan, err := r.Plan(ctx)
		if err != nil {
			return err
		}

		printPlan(cmd.OutOrStdout(), plan)

		if plan.Empty() {
			return nil
		}

		return r.Apply(ctx, plan)
	},
}

var fleetWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Continuously reconcile Orchard to match fleet.yaml.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		r, err := newReconciler(cfg)
		if err != nil {
			return err
		}

		interval, err := cmd.Flags().GetDuration("interval")
		if err != nil {
			return err
		}

		return r.Run(cmd.Context(), interval)
	},
}

var fleetStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show pool/worker/vm/status/age/ttl for the fleet.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}

		vms, err := client.ListVMs(cmd.Context())
		if err != nil {
			return err
		}

		sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "POOL\tWORKER\tVM\tSTATUS\tAGE\tTTL")

		for _, vm := range vms {
			pool, worker := fleet.PoolAndHost(vm)

			age := "-"
			if !vm.CreatedAt.IsZero() {
				age = time.Since(vm.CreatedAt).Round(time.Second).String()
			}

			ttl := "-"
			if vm.TTL > 0 {
				ttl = vm.TTL.String()
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", pool, worker, vm.Name, vm.Status, age, ttl)
		}

		if err := w.Flush(); err != nil {
			return err
		}

		printReconcileStatus(cmd, cfg)

		return nil
	},
}

// printReconcileStatus prints one best-effort "last reconcile: …" line summarizing the running
// `grove serve`'s background fleet reconciler loop (see internal/cli/serve.go), fetched over the
// API. `fleet status` itself talks to Orchard directly and doesn't need a server to work at all,
// so a server that isn't configured or isn't reachable just means this line is skipped — never a
// hard failure of the command.
func printReconcileStatus(cmd *cobra.Command, cfg *config.Config) {
	if cfg.Server.URL == "" {
		return
	}

	client := apiclient.New(cfg.Server.URL, cfg.Server.Token)
	status, err := client.FleetReconcile(cmd.Context())
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "last reconcile: unavailable (%v)\n", err)
		return
	}

	if !status.Enabled {
		fmt.Fprintln(cmd.OutOrStdout(), "last reconcile: fleet reconciler disabled on the server")
		return
	}

	if status.LastRunAt.IsZero() {
		fmt.Fprintln(cmd.OutOrStdout(), "last reconcile: not yet run")
		return
	}

	line := fmt.Sprintf("last reconcile: %s ago (every %s)", time.Since(status.LastRunAt).Round(time.Second), status.Interval)
	if status.LastError != "" {
		line += fmt.Sprintf(", last error: %s", status.LastError)
	} else if len(status.LastPlan.Creates) > 0 || len(status.LastPlan.Deletes) > 0 {
		line += fmt.Sprintf(", last plan: +%d/-%d", len(status.LastPlan.Creates), len(status.LastPlan.Deletes))
	}
	fmt.Fprintln(cmd.OutOrStdout(), line)
}

func newReconciler(cfg *config.Config) (*fleet.Reconciler, error) {
	if cfg.Fleet == "" {
		return nil, fmt.Errorf("fleet is not configured; run `grove config set fleet <path/to/fleet.yaml>`")
	}

	spec, err := fleet.Load(cfg.Fleet)
	if err != nil {
		return nil, err
	}

	client, err := newOrchardClient(cfg)
	if err != nil {
		return nil, err
	}

	return &fleet.Reconciler{
		Client: client,
		Spec:   spec,
		Options: fleet.Options{
			TailscaleAuthKey: cfg.Tailscale.AuthKey,
		},
	}, nil
}

func printPlan(w io.Writer, plan fleet.Plan) {
	if plan.Empty() {
		fmt.Fprintln(w, "up to date")
		return
	}

	for _, d := range plan.Deletes {
		fmt.Fprintf(w, "- delete %-30s pool=%s worker=%s (%s)\n", d.Name, d.Pool, d.Worker, d.Reason)
	}

	for _, c := range plan.Creates {
		fmt.Fprintf(w, "+ create %-30s pool=%s worker=%s\n", c.Spec.Name, c.Pool, c.Worker)
	}
}
