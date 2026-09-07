package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/spf13/cobra"
)

func init() {
	Root.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Ping Orchard and Nomad and print fleet counts.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
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
		}

		if nomadErr == nil {
			printNomadCounts(ctx, out, nomadClient)
		}

		if orchardErr != nil || nomadErr != nil {
			return fmt.Errorf("one or more control-plane checks failed")
		}

		return nil
	},
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
	fmt.Fprintf(out, "nomad nodes: %d\n", len(nodes))
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

	return nomad.New(cfg.Nomad.URL, cfg.Nomad.Token)
}
