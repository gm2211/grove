package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
)

// Retry budget for `nomad acl bootstrap` while the freshly started server elects itself leader.
// Variables so tests don't sleep.
var (
	nomadACLBootstrapAttempts = 30
	nomadACLBootstrapWait     = 2 * time.Second
)

// nomadACLBootstrapStep bootstraps Nomad's ACL system once the server is running with
// `acl { enabled = true }` (see RenderNomadServerConfig) and stores the management token in
// config.yaml's nomad.token, which grove serve and the CLI already send as X-Nomad-Token. The
// token comes back on stdout, never argv. A control plane installed while ACLs were off has its
// Nomad restarted once so the re-rendered server.hcl takes effect.
func nomadACLBootstrapStep(r Runner, opts Options, isDarwin bool) Step {
	cfgDir := opts.ConfigDir()
	return Step{
		Name:        "nomad-acl-bootstrap",
		Description: "Bootstrap Nomad ACLs and store the management token in config.yaml (nomad.token); anonymous requests to Nomad get nothing.",
		Check: func(ctx context.Context) (bool, error) {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil {
				return false, err
			}
			return cfg.Nomad.Token != "", nil
		},
		Apply: func(ctx context.Context) error {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil {
				return err
			}
			if cfg.Nomad.URL == "" {
				return errors.New("config.yaml has no nomad.url; the config:grove-config-yaml step must run first")
			}
			restarted := false
			var lastErr error
			for attempt := 0; attempt < nomadACLBootstrapAttempts; attempt++ {
				if attempt > 0 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(nomadACLBootstrapWait):
					}
				}
				stdout, stderr, err := r.Run(ctx, "nomad", "acl", "bootstrap", "-address="+cfg.Nomad.URL, "-json")
				if err == nil {
					var out struct{ SecretID string }
					if jerr := json.Unmarshal([]byte(stdout), &out); jerr != nil || out.SecretID == "" {
						return errors.New("nomad acl bootstrap returned no management token")
					}
					return updateConfig(cfgDir, func(cfg *config.Config) { cfg.Nomad.Token = out.SecretID })
				}
				msg := stderr + " " + err.Error()
				switch {
				case strings.Contains(msg, "bootstrap already done"):
					return errors.New("Nomad's ACLs were already bootstrapped but config.yaml has no nomad.token: " +
						"set the existing management token with `grove config set nomad.token <secret>`, " +
						"or reset the bootstrap (Nomad docs: \"Reset the ACL system\") and re-run this install")
				case strings.Contains(msg, "ACL support disabled") && !restarted:
					if err := restartNomadService(ctx, r, isDarwin); err != nil {
						return fmt.Errorf("restart Nomad to turn ACLs on: %w", err)
					}
					restarted = true
				}
				lastErr = fmt.Errorf("nomad acl bootstrap: %s", strings.TrimSpace(msg))
			}
			return lastErr
		},
	}
}

func restartNomadService(ctx context.Context, r Runner, isDarwin bool) error {
	if isDarwin {
		_, _, err := r.Run(ctx, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/com.grove.nomad", os.Getuid()))
		return err
	}
	_, _, err := r.Run(ctx, "systemctl", "--user", "restart", "grove-nomad.service")
	return err
}
