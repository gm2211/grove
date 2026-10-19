package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/sandbox"
)

// sandboxCmd groups the commands grove's own Nomad jobs run on the control plane. Hidden: nothing
// here is meant to be typed by hand.
var sandboxCmd = &cobra.Command{
	Use:    "sandbox",
	Short:  "Drive jobs in sealed VMs (run by grove's Nomad jobs, not by hand)",
	Hidden: true,
}

type sandboxRunFlags struct {
	kind, pool, image, payload, os string
	slots, cpu, memory             int
}

func init() {
	var f sandboxRunFlags
	run := &cobra.Command{
		Use:   "run",
		Short: "Run one dispatched job in a sealed VM of its pool (the entrypoint of nomad/jobs/sandbox.nomad.hcl)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSandbox(cmd.Context(), f)
		},
	}
	run.Flags().StringVar(&f.kind, "kind", "", "job kind: build, agent or shell")
	run.Flags().StringVar(&f.pool, "pool", "", "fleet.yaml pool whose sealed VMs run the job")
	run.Flags().StringVar(&f.image, "image", "", "container image the job runs in (ignored for --os darwin)")
	run.Flags().StringVar(&f.os, "os", sandbox.OSLinux, "the pool's guest OS: linux (jobs run in docker containers) or darwin (jobs run as the VM's job user, one per VM)")
	run.Flags().StringVar(&f.payload, "payload", "", "the Nomad dispatch payload file; deleted once read")
	run.Flags().IntVar(&f.slots, "slots", 1, "how many jobs one VM of the pool runs at once (always 1 for --os darwin)")
	run.Flags().IntVar(&f.cpu, "cpu", 0, "the job's CPU in MHz (2000 per core); 0 leaves it unlimited")
	run.Flags().IntVar(&f.memory, "memory", 0, "the job's memory in MiB; 0 leaves it unlimited")
	sandboxCmd.AddCommand(run)
	Root.AddCommand(sandboxCmd)
}

func runSandbox(ctx context.Context, f sandboxRunFlags) error {
	// The payload can hold the private-repo clone token: read it, then remove it before anything
	// else can go wrong.
	payload, err := os.ReadFile(f.payload)
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	if err := os.Remove(f.payload); err != nil {
		slog.Warn("sandbox: could not delete the payload file", "err", err)
	}
	token, script := sandbox.ParsePayload(f.kind, payload)

	req := sandbox.Request{
		Kind:           f.kind,
		Pool:           f.pool,
		OS:             f.os,
		AllocID:        os.Getenv("NOMAD_ALLOC_ID"),
		Image:          f.image,
		CPU:            f.cpu,
		Memory:         f.memory,
		Repo:           os.Getenv("NOMAD_META_repo"),
		Ref:            os.Getenv("NOMAD_META_ref"),
		CloneToken:     token,
		Script:         script,
		ArtifactPrefix: os.Getenv("NOMAD_META_artifact_prefix"),
	}
	if raw := os.Getenv("NOMAD_META_env_json"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &req.Env); err != nil {
			return fmt.Errorf("parse env_json: %w", err)
		}
	}
	if raw := os.Getenv("NOMAD_META_timeout_seconds"); raw != "" {
		secs, err := strconv.ParseFloat(raw, 64)
		if err != nil || secs <= 0 {
			return fmt.Errorf("invalid timeout_seconds %q", raw)
		}
		req.Timeout = time.Duration(secs * float64(time.Second))
	}

	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	oc, err := newOrchardClient(cfg)
	if err != nil {
		return err
	}
	var uploader artifacts.Uploader
	if ac, err := artifacts.New(cfg.Artifacts); err == nil {
		uploader, _ = ac.(artifacts.Uploader)
	} else {
		slog.Warn("sandbox: artifact store unavailable; build artifacts won't be collected", "err", err)
	}

	runner := &sandbox.Runner{
		Orchard:  oc,
		Uploader: uploader,
		Slots:    sandbox.Slots{Dir: sandboxSlotDir(), PerVM: sandboxSlots(f)},
		WorkDir:  os.Getenv("NOMAD_TASK_DIR"),
	}
	if f.kind == sandbox.KindAgent {
		// The sandbox template gives an agent a 10m kill_timeout; leave time to collect its
		// output and remove its container.
		runner.StopGrace = 8 * time.Minute
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if code := runner.Run(ctx, req); code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// sandboxSlots is the per-VM slot count: a macOS VM runs one job and is then deleted.
func sandboxSlots(f sandboxRunFlags) int {
	if f.os == sandbox.OSDarwin {
		return 1
	}
	return f.slots
}

// sandboxSlotDir holds the per-VM slot locks every `grove sandbox run` on this machine shares.
func sandboxSlotDir() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "grove", "sandbox-slots")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "grove", "sandbox-slots")
	}
	return filepath.Join(os.TempDir(), "grove-sandbox-slots")
}
