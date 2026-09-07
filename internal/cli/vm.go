package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/gm2211/grove/internal/orchard"
	"github.com/spf13/cobra"
)

func init() {
	Root.AddCommand(vmCmd)
	vmCmd.AddCommand(vmLsCmd)
	vmCmd.AddCommand(vmGetCmd)
	vmCmd.AddCommand(vmDeleteCmd)
	vmCmd.AddCommand(vmExecCmd)
}

var vmCmd = &cobra.Command{
	Use:   "vm",
	Short: "Inspect and control individual VMs.",
}

var vmLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List VMs.",
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
		fmt.Fprintln(w, "NAME\tWORKER\tSTATUS\tIMAGE\tAGE")

		for _, vm := range vms {
			age := "-"
			if !vm.CreatedAt.IsZero() {
				age = time.Since(vm.CreatedAt).Round(time.Second).String()
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", vm.Name, vm.Worker, vm.Status, vm.Image, age)
		}

		return w.Flush()
	},
}

var vmGetCmd = &cobra.Command{
	Use:   "get <vm>",
	Short: "Show details for a single VM.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}

		vm, err := client.GetVM(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "name\t%s\n", vm.Name)
		fmt.Fprintf(w, "worker\t%s\n", vm.Worker)
		fmt.Fprintf(w, "status\t%s\n", vm.Status)

		if vm.StatusMessage != "" {
			fmt.Fprintf(w, "message\t%s\n", vm.StatusMessage)
		}

		fmt.Fprintf(w, "image\t%s\n", vm.Image)
		fmt.Fprintf(w, "cpu\t%d\n", vm.CPU)
		fmt.Fprintf(w, "memory\t%d MiB\n", vm.Memory)
		fmt.Fprintf(w, "restarts\t%d\n", vm.RestartCount)

		if !vm.CreatedAt.IsZero() {
			fmt.Fprintf(w, "created\t%s\n", vm.CreatedAt.Format(time.RFC3339))
		}

		labelKeys := make([]string, 0, len(vm.Labels))
		for k := range vm.Labels {
			labelKeys = append(labelKeys, k)
		}
		sort.Strings(labelKeys)

		for _, k := range labelKeys {
			fmt.Fprintf(w, "label:%s\t%s\n", k, vm.Labels[k])
		}

		return w.Flush()
	},
}

var vmDeleteCmd = &cobra.Command{
	Use:   "delete <vm>",
	Short: "Delete a VM.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}

		if err := client.DeleteVM(cmd.Context(), args[0]); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])

		return nil
	},
}

var vmExecCmd = &cobra.Command{
	Use:   "exec <vm> -- <command...>",
	Short: "Run a command inside a VM.",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		vmName, command, err := splitExecArgs(cmd, args)
		if err != nil {
			return err
		}

		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}

		session, err := client.Exec(cmd.Context(), vmName, command, orchard.ExecOptions{
			Wait: 30 * time.Second,
		})
		if err != nil {
			return err
		}
		defer session.Close()

		if _, err := io.Copy(cmd.OutOrStdout(), session.Output()); err != nil {
			return err
		}

		code, err := session.Wait(cmd.Context())
		if err != nil {
			return err
		}

		if code != 0 {
			return fmt.Errorf("command exited %d", code)
		}

		return nil
	},
}

// splitExecArgs separates the VM name from the command to run, using the "--" that separates
// grove's own flags from the remote command when present, and falling back to treating the
// first argument as the VM name otherwise.
func splitExecArgs(cmd *cobra.Command, args []string) (vm string, command []string, err error) {
	if dash := cmd.ArgsLenAtDash(); dash > 0 {
		return args[0], args[dash:], nil
	}

	if len(args) < 2 {
		return "", nil, fmt.Errorf("usage: grove vm exec <vm> -- <command...>")
	}

	return args[0], args[1:], nil
}
