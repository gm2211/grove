package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/server"
	"github.com/gm2211/grove/internal/wire"
)

func init() {
	var listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the grove control-plane HTTP API + embedded UI.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd.Context(), listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "listen address, e.g. 0.0.0.0:6120 (default: server.listen from config, else 127.0.0.1:6120)")
	Root.AddCommand(cmd)
}

func runServe(ctx context.Context, listen string) error {
	cfg, cfgPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	addr := listen
	if addr == "" {
		addr = cfg.Server.Listen
	}
	if addr == "" {
		addr = "127.0.0.1:6120"
	}

	oc, err := wire.NewOrchardClient(cfg.Orchard)
	if err != nil {
		return fmt.Errorf("orchard client: %w", err)
	}
	nc, err := wire.NewNomadClient(cfg.Nomad)
	if err != nil {
		return fmt.Errorf("nomad client: %w", err)
	}

	ac, err := wire.NewArtifactsClient(cfg.Artifacts)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotConfigured) {
			slog.Info("serve: no artifact store configured; job artifact download/listing is disabled", "err", err)
			ac = nil
		} else {
			return fmt.Errorf("artifacts client: %w", err)
		}
	}

	pools, perr := poolConfigs(cfg)
	if perr != nil {
		slog.Warn("serve: could not determine pools for EnsureJobs; skipping job registration", "config", cfgPath, "err", perr)
	} else if len(pools) == 0 {
		slog.Warn("serve: no pools found in fleet spec; no parameterized jobs registered", "fleet", cfg.Fleet)
	}

	ds, err := dispatch.New(nc, dispatch.Options{ArtifactsBase: cfg.Artifacts.Bucket, Artifacts: ac, Pools: pools})
	if err != nil {
		return fmt.Errorf("dispatch service: %w", err)
	}

	if len(pools) > 0 {
		if err := dispatch.EnsureJobs(ctx, nc, pools); err != nil {
			slog.Warn("serve: EnsureJobs failed; continuing to serve anyway", "err", err)
		}
	}

	// Periodically reconcile every non-terminal job's status against Nomad, so an orphaned job
	// (its dispatched Nomad job 404s — e.g. a dev Nomad agent restarted with its data dir wiped) or
	// one stuck pending past dispatch.Options.PendingTimeout gets marked lost even if nothing polls
	// it via Get/List in the meantime. Runs for the life of the process; a failed tick is logged by
	// RunReconciler itself and doesn't stop the sweep.
	go func() {
		if err := ds.RunReconciler(ctx, dispatch.DefaultReconcileInterval); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("serve: job reconciler sweep stopped", "err", err)
		}
	}()

	srv := server.New(oc, nc, ds, ac, server.Options{Token: cfg.Server.Token, Version: Version})
	slog.Info("grove serve: listening", "addr", addr)
	return http.ListenAndServe(addr, srv)
}

// poolConfigs reads cfg.Fleet (fleet.yaml) and returns the configured pools' EnsureJobs config, so
// `grove serve` can register grove-<kind>-<pool> jobs for each of them at startup. Returns (nil,
// nil) if no fleet spec is configured.
func poolConfigs(cfg *config.Config) ([]dispatch.PoolConfig, error) {
	if cfg.Fleet == "" {
		return nil, nil
	}
	data, err := os.ReadFile(cfg.Fleet)
	if err != nil {
		return nil, fmt.Errorf("read fleet spec %s: %w", cfg.Fleet, err)
	}
	var spec fleet.Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse fleet spec %s: %w", cfg.Fleet, err)
	}
	pools := make([]dispatch.PoolConfig, 0, len(spec.Pools))
	for _, p := range spec.Pools {
		pools = append(pools, dispatch.PoolConfig{
			Name:              p.Name,
			CPU:               int(p.JobCPUOrDefault()),
			Memory:            int(p.JobMemoryOrDefault()),
			AllowDockerSocket: p.AllowDockerSocket,
		})
	}
	return pools, nil
}
