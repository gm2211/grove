package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

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

		return w.Flush()
	},
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
