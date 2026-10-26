package install

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/config"
)

// brokerLabel is the LaunchAgent that runs a worker's credential broker (`grove broker`).
const brokerLabel = "com.grove.broker"

// groveExecutable is the grove binary the broker's LaunchAgent runs: grove on PATH (a Homebrew
// symlink, which keeps working across upgrades), or else the binary running the install.
func groveExecutable(opts Options) string {
	if p, err := opts.lookPath()("grove"); err == nil {
		return p
	}
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "grove"
}

// brokerLaunchAgentPlist renders the LaunchAgent that supervises `grove broker`.
func brokerLaunchAgentPlist(opts Options, groveBin string) (string, error) {
	args := []string{"broker"}
	if opts.ServerURL != "" {
		args = append(args, "--server", opts.ServerURL)
	}
	return RenderLaunchAgent(LaunchAgentSpec{
		Label:      brokerLabel,
		Program:    groveBin,
		Args:       args,
		WorkingDir: opts.homeDir(),
		KeepAlive:  true,
		RunAtLoad:  true,
		StdoutPath: filepath.Join(opts.LogDir(), "broker.log"),
		StderrPath: filepath.Join(opts.LogDir(), "broker.err.log"),
	})
}

// brokerSteps install a worker's credential broker: its credential in Keychain, when setup brought
// one, and the LaunchAgent that runs it. Without a credential the broker still runs (job VMs get
// its CA and plain tunnels) and says in its log, and in `grove doctor`, that it can't sync.
func brokerSteps(r Runner, opts Options) []Step {
	uid := fmt.Sprint(os.Getuid())
	label := "gui/" + uid + "/" + brokerLabel
	plistPath := filepath.Join(opts.LaunchAgentsDir(), brokerLabel+".plist")
	groveBin := groveExecutable(opts)

	var steps []Step
	if opts.BrokerToken != "" {
		steps = append(steps, Step{
			Name:        "broker-keychain",
			Description: "Store the broker's credential (broker scope only) in macOS Keychain; it never appears in process arguments or LaunchAgent files.",
			Check: func(ctx context.Context) (bool, error) {
				stdout, _, err := r.Run(ctx, "/usr/bin/security", "find-generic-password", "-a", config.BrokerTokenKeychainAccount, "-s", config.BrokerTokenKeychainService, "-w")
				return err == nil && strings.TrimSpace(stdout) == opts.BrokerToken, nil
			},
			Apply: func(ctx context.Context) error {
				if err := StoreKeychainSecret(ctx, r, config.BrokerTokenKeychainAccount, config.BrokerTokenKeychainService, opts.BrokerToken); err != nil {
					return err
				}
				// A running broker keeps the credential it read; restart it onto the new one. First
				// installs have nothing loaded yet, and launchagent-load:broker starts it.
				_, _, _ = r.Run(ctx, "launchctl", "kickstart", "-k", label)
				return nil
			},
		})
	}
	return append(steps,
		Step{
			Name: "launchagent:broker",
			Description: fmt.Sprintf("Render %s (KeepAlive+RunAtLoad, logs under %s) running `%s broker`, the proxy job VMs use: it swaps their placeholders for real tokens.",
				plistPath, opts.LogDir(), groveBin),
			Check: fileHasContent(plistPath, func() (string, error) { return brokerLaunchAgentPlist(opts, groveBin) }),
			Apply: func(ctx context.Context) error {
				content, err := brokerLaunchAgentPlist(opts, groveBin)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
					return err
				}
				if err := os.MkdirAll(opts.LogDir(), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
					return err
				}
				// Unload the old definition, if there is one, so launchagent-load:broker starts the
				// new one. Not loaded is fine.
				_, _, _ = r.Run(ctx, "launchctl", "bootout", label)
				return nil
			},
		},
		Step{
			Name:        "launchagent-load:broker",
			Description: fmt.Sprintf("Load %s in gui/%s.", plistPath, uid),
			Check: func(ctx context.Context) (bool, error) {
				stdout, _, err := r.Run(ctx, "launchctl", "print", label)
				return err == nil && strings.Contains(stdout, groveBin), nil
			},
			Apply: func(ctx context.Context) error {
				_, _, _ = r.Run(ctx, "launchctl", "bootout", label)
				_, _, err := r.Run(ctx, "launchctl", "bootstrap", "gui/"+uid, plistPath)
				return err
			},
		},
	)
}

// VMNetGateway is the host's address on vmnet's shared network, where the broker listens:
// Shared_Net_Address from the Mac's vmnet preferences when set, broker.DefaultGateway otherwise.
func VMNetGateway(ctx context.Context, r Runner) (netip.Addr, error) {
	out, _, err := r.Run(ctx, "/usr/bin/defaults", "read", "/Library/Preferences/SystemConfiguration/com.apple.vmnet", "Shared_Net_Address")
	if err != nil {
		return broker.DefaultGateway, nil // not set: vmnet uses its default
	}
	a, err := netip.ParseAddr(strings.TrimSpace(out))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("vmnet Shared_Net_Address %q: %w", strings.TrimSpace(out), err)
	}
	return a, nil
}

// checkBroker reports on a worker's credential broker: its LaunchAgent, its credential and, once
// the VM bridge exists, whether it answers and has synced with the control plane.
func checkBroker(ctx context.Context, r Runner, opts Options) CheckResult {
	const name = "broker"
	setup := "run `grove setup` on this Mac"
	if _, err := os.Stat(filepath.Join(opts.LaunchAgentsDir(), brokerLabel+".plist")); err != nil {
		return CheckResult{Name: name, OK: false, Detail: "not installed, so jobs here can't use credentials", Remediation: setup}
	}
	// Without -w, security prints the item's attributes, never the secret.
	if _, _, err := r.Run(ctx, "/usr/bin/security", "find-generic-password", "-a", config.BrokerTokenKeychainAccount, "-s", config.BrokerTokenKeychainService); err != nil {
		return CheckResult{Name: name, OK: false, Detail: "no broker credential in Keychain", Remediation: setup}
	}
	if _, _, err := r.Run(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), brokerLabel)); err != nil {
		return CheckResult{Name: name, OK: false, Detail: brokerLabel + " is not loaded", Remediation: setup}
	}
	gw, err := VMNetGateway(ctx, r)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error()}
	}
	if ok, _ := broker.HasLocalAddr(gw); !ok {
		return CheckResult{Name: name, OK: true, Detail: fmt.Sprintf("loaded; listens on %s once the first VM creates the bridge", netip.AddrPortFrom(gw, broker.Port))}
	}
	return brokerHealth(ctx, "http://"+netip.AddrPortFrom(gw, broker.Port).String())
}

// brokerHealth reads the broker's /healthz at base.
func brokerHealth(ctx context.Context, base string) CheckResult {
	const name = "broker"
	logs := "see ~/Library/Logs/grove/broker.err.log"
	ctx, cancel := context.WithTimeout(ctx, httpCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error()}
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}} // the broker is local: never via a proxy
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: "not answering: " + err.Error(), Remediation: logs}
	}
	defer resp.Body.Close()
	var health struct {
		Entries int   `json:"entries"`
		Synced  *bool `json:"synced"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&health) != nil {
		return CheckResult{Name: name, OK: false, Detail: fmt.Sprintf("GET %s/healthz: HTTP %d", base, resp.StatusCode), Remediation: logs}
	}
	if health.Synced != nil && !*health.Synced {
		return CheckResult{Name: name, OK: false, Detail: fmt.Sprintf("answering on %s, but its last sync with the control plane failed (%d entries kept)", base, health.Entries), Remediation: logs}
	}
	return CheckResult{Name: name, OK: true, Detail: fmt.Sprintf("answering on %s, synced, %d entries", base, health.Entries)}
}
