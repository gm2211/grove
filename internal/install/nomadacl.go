package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/nomad"
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
				args := []string{"acl", "bootstrap", "-address=" + cfg.Nomad.URL}
				args = append(args, nomadCLITLSFlags(cfg.Nomad)...)
				stdout, stderr, err := r.Run(ctx, "nomad", append(args, "-json")...)
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
	return restartUserService(ctx, r, isDarwin, "nomad")
}

// restartUserService restarts one of the control plane's supervised services (com.grove.<name>
// on macOS, grove-<name>.service on Linux).
func restartUserService(ctx context.Context, r Runner, isDarwin bool, name string) error {
	if isDarwin {
		_, _, err := r.Run(ctx, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/com.grove.%s", os.Getuid(), name))
		return err
	}
	_, _, err := r.Run(ctx, "systemctl", "--user", "restart", "grove-"+name+".service")
	return err
}

// nomadSandboxIntroTTL bounds the sandbox-host client's introduction token: it is used within
// seconds of being written, by the restart right after.
const nomadSandboxIntroTTL = 30 * time.Minute

// nomadSandboxIntroStep lets the control plane's sandbox-host client (see
// RenderNomadSandboxClientConfig) register under strict client introduction: it mints a token
// bound to SandboxClientNodeName, leaves it in the client's state directory (mode 0600), and
// restarts the agent so it registers with it. Once registered the client authenticates with the
// node identity the server issued it, so the step is a no-op on later runs.
func nomadSandboxIntroStep(r Runner, opts Options, isDarwin bool) Step {
	cfgDir := opts.ConfigDir()
	tokenPath := filepath.Join(nomadSandboxDataDir(cfgDir), "client", "intro_token.jwt")
	registered := func(ctx context.Context) (bool, error) {
		cfg, err := loadOrInitConfig(cfgDir)
		if err != nil || cfg.Nomad.URL == "" || cfg.Nomad.Token == "" {
			return false, err
		}
		nc, err := opts.nomadClient(cfg.Nomad)
		if err != nil {
			return false, nil
		}
		ctx, cancel := context.WithTimeout(ctx, httpCheckTimeout)
		defer cancel()
		nodes, err := nc.ListNodes(ctx)
		if err != nil {
			return false, nil
		}
		for _, n := range nodes {
			if n.Name == SandboxClientNodeName && n.Status == "ready" {
				return true, nil
			}
		}
		return false, nil
	}
	return Step{
		Name: "nomad-sandbox-client-intro",
		Description: fmt.Sprintf("Mint a Nomad introduction token bound to %s, write it to %s (0600) and restart the sandbox-host client so it registers.",
			SandboxClientNodeName, tokenPath),
		Check: registered,
		Apply: func(ctx context.Context) error {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil {
				return err
			}
			if cfg.Nomad.Token == "" {
				return errors.New("config.yaml has no nomad.token; the nomad-acl-bootstrap step must run first")
			}
			nc, err := opts.nomadClient(cfg.Nomad)
			if err != nil {
				return err
			}
			intros, ok := nomad.AsNodeIntros(nc)
			if !ok {
				return errors.New("nomad client cannot mint introduction tokens")
			}
			var tok string
			for attempt := 0; attempt < nomadACLBootstrapAttempts; attempt++ {
				if attempt > 0 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(nomadACLBootstrapWait):
					}
				}
				if tok, err = intros.CreateIntroToken(ctx, SandboxClientNodeName, "", nomadSandboxIntroTTL); err == nil {
					break
				}
			}
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
				return err
			}
			if err := writeFile(tokenPath, tok); err != nil {
				return err
			}
			return restartUserService(ctx, r, isDarwin, "nomad-sandbox-client")
		},
	}
}

// nomadCLITLSFlags are the `nomad` CLI flags for config.yaml's Nomad TLS files: paths only, never
// key material.
func nomadCLITLSFlags(ep config.Endpoint) []string {
	var flags []string
	if ep.CACert != "" {
		flags = append(flags, "-ca-cert="+ep.CACert)
	}
	if ep.ClientCert != "" && ep.ClientKey != "" {
		flags = append(flags, "-client-cert="+ep.ClientCert, "-client-key="+ep.ClientKey)
	}
	return flags
}
