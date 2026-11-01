package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/config"
)

// brokerLabel is the LaunchDaemon that runs a worker's credential broker (`grove broker`).
const brokerLabel = "com.grove.broker"

// groveExecutable is the grove binary the broker's LaunchDaemon runs: grove on PATH (a Homebrew
// symlink, which keeps working across upgrades), or else the binary running the install.
// Options.GroveBin, when set, wins: `grove setup --user` names the same binary on both sides.
func groveExecutable(opts Options) string {
	if opts.GroveBin != "" {
		return opts.GroveBin
	}
	if p, err := opts.lookPath()("grove"); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			return abs
		}
	}
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "grove"
}

// brokerDaemonPlist renders the LaunchDaemon, run as the worker user, that supervises `grove broker`.
func brokerDaemonPlist(opts Options, groveBin string) (string, error) {
	args := []string{"broker"}
	if opts.ServerURL != "" {
		args = append(args, "--server", opts.ServerURL)
	}
	return daemonPlist(opts, LaunchAgentSpec{
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

// brokerSteps install a worker's credential broker: its credential in a 0600 file, when setup
// brought one (or an older grove left one in Keychain), and the LaunchDaemon plist that runs it,
// staged for the worker-daemons step to install. Without a credential the broker still runs (job
// VMs get its CA and plain tunnels) and says in its log, and in `grove doctor`, that it can't sync.
func brokerSteps(r Runner, opts Options, d daemon) []Step {
	path := credentialPath(opts, brokerCredentialName)
	return []Step{
		{
			Name:        "broker-credential",
			Description: fmt.Sprintf("Store the broker's credential (broker scope only) in %s (0600, this user's); it never appears in process arguments or launchd plists. A re-run without one keeps it, moving it out of the login Keychain if that is where an older grove put it.", path),
			Check: func(ctx context.Context) (bool, error) {
				if secretFileIs(path, opts.BrokerToken) {
					return true, nil
				}
				if opts.BrokerToken != "" {
					return false, nil
				}
				// Nothing given, nothing stored: satisfied unless an older grove left one in Keychain.
				token, err := keychainBrokerToken(ctx, r)
				return err != nil || token == "", nil
			},
			Apply: func(ctx context.Context) error {
				token := opts.BrokerToken
				if token == "" {
					var err error
					if token, err = keychainBrokerToken(ctx, r); err != nil || token == "" {
						return fmt.Errorf("move the broker credential out of Keychain: %v (run `grove setup` again, or unlock the login Keychain with `security unlock-keychain`)", err)
					}
				}
				if err := secretLine("the broker credential", token); err != nil {
					return err
				}
				if err := writeSecretFile(path, token+"\n"); err != nil {
					return err
				}
				// A running broker keeps the credential it read; restart it onto the new one. First
				// installs have nothing running yet, and the worker-daemons step starts it.
				restartDaemon(ctx, r, brokerLabel)
				return nil
			},
		},
		daemonStageStep(opts, d),
	}
}

// keychainBrokerToken reads the broker credential an older grove stored in the login Keychain.
func keychainBrokerToken(ctx context.Context, r Runner) (string, error) {
	stdout, _, err := r.Run(ctx, "/usr/bin/security", "find-generic-password", "-a", config.BrokerTokenKeychainAccount, "-s", config.BrokerTokenKeychainService, "-w")
	return strings.TrimSpace(stdout), err
}

// VMNetGateway is the host's address on vmnet's shared network, where the broker listens:
// Shared_Net_Address from the Mac's vmnet preferences when set, broker.DefaultGateway otherwise.
func VMNetGateway(ctx context.Context, r Runner) (netip.Addr, error) {
	out, _, err := r.Run(ctx, "/usr/bin/defaults", "read", "/Library/Preferences/SystemConfiguration/com.apple.vmnet", "Shared_Net_Address")
	if err != nil || strings.TrimSpace(out) == "" {
		return broker.DefaultGateway, nil // not set: vmnet uses its default
	}
	a, err := netip.ParseAddr(strings.TrimSpace(out))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("vmnet Shared_Net_Address %q: %w", strings.TrimSpace(out), err)
	}
	return a, nil
}

// checkBroker reports on a worker's credential broker: its LaunchDaemon, its credential and, once
// the VM bridge exists, whether it answers and has synced with the control plane.
func checkBroker(ctx context.Context, r Runner, opts Options) CheckResult {
	const name = "broker"
	setup := "run `grove setup` on this Mac"
	d := workerDaemons(opts)[1]
	_, installed := daemonPaths(opts, d.Label)
	if _, err := os.Stat(installed); err != nil {
		if _, agentErr := os.Stat(legacyAgentPlist(opts, d.Label)); agentErr == nil {
			return CheckResult{Name: name, OK: false, Detail: "still a LaunchAgent, which stops when this user logs out and does not start at boot", Remediation: daemonsRemediation(opts)}
		}
		return CheckResult{Name: name, OK: false, Detail: "not installed, so jobs here can't use credentials", Remediation: setup}
	}
	credential := credentialPath(opts, brokerCredentialName)
	switch token, err := readSecretFile(credential); {
	case errors.Is(err, os.ErrNotExist):
		return CheckResult{Name: name, OK: false, Detail: "no broker credential file (" + credential + ")", Remediation: setup}
	case err != nil:
		return CheckResult{Name: name, OK: false, Detail: err.Error(), Remediation: "run `chmod 600 " + credential + "`, or " + setup}
	case token == "":
		return CheckResult{Name: name, OK: false, Detail: "the broker credential file " + credential + " is empty", Remediation: setup}
	}
	if info, err := os.Lstat(credential); err == nil {
		if err := checkOwnedByUs(credential, info); err != nil {
			return CheckResult{Name: name, OK: false, Detail: err.Error(), Remediation: setup}
		}
	}
	if !daemonLoaded(ctx, r, d) {
		return CheckResult{Name: name, OK: false, Detail: brokerLabel + " is not loaded in the system domain", Remediation: daemonsRemediation(opts)}
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
