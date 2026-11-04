package install

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/fleet"
)

const (
	orchardControllerLabel   = "com.grove.orchard-controller"
	orchardControllerService = "grove-orchard-controller.service"
	// bootstrapAdminTokenEnv is the variable Orchard's controller reads to create a full-admin
	// "bootstrap-admin" service account with a caller-chosen token. Grove never sets it.
	bootstrapAdminTokenEnv = "ORCHARD_BOOTSTRAP_ADMIN_TOKEN"
)

// OrchardControllerArgs is the argument list of the supervised `orchard controller run`. Orchard
// reads no environment variable for any of this (only PORT for its default port): launchd and
// systemd do not source the env file, so without these flags a restarted controller listens on
// ":6120" on every interface with auto-generated TLS, and every worker and grove client (which use
// plain http://<listen>) fails to connect. Grove's config.yaml always says http://, hence
// --insecure-no-tls: the controller sits on the tailnet. The host-dirs flag is wanted only when
// a pool shares a host path (hostXcode); Orchard refuses path-based host directories without it.
func OrchardControllerArgs(listen string, hostDirs bool) []string {
	args := []string{"controller", "run", "--listen", listen, "--insecure-no-tls"}
	if hostDirs {
		args = append(args, "--insecure-allow-host-dirs")
	}
	return args
}

// fleetNeedsHostDirs reports whether the fleet spec at path has a pool that mounts a host
// directory. A missing or invalid spec means no.
func fleetNeedsHostDirs(path string) bool {
	spec, err := fleet.Load(path)
	if err != nil {
		return false
	}
	for _, p := range spec.Pools {
		if p.HostXcode != nil {
			return true
		}
	}
	return false
}

// runningUnit is what the supervisor says a running service was started with.
type runningUnit struct {
	PID     int
	Argv    []string // program first
	EnvKeys []string // names only: values are never kept
}

// parseLaunchctlPrint extracts the pid, argv and environment variable names from
// `launchctl print gui/<uid>/<label>`.
func parseLaunchctlPrint(out string) runningUnit {
	var u runningUnit
	block := ""
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case block != "":
			if line == "}" {
				block = ""
				continue
			}
			switch block {
			case "arguments":
				u.Argv = append(u.Argv, line)
			case "environment":
				if k, _, ok := strings.Cut(line, "=>"); ok {
					u.EnvKeys = append(u.EnvKeys, strings.TrimSpace(k))
				}
			}
		case line == "arguments = {":
			block = "arguments"
		case strings.HasSuffix(line, "environment = {"):
			block = "environment"
		case strings.HasPrefix(line, "pid = "):
			u.PID, _ = strconv.Atoi(strings.TrimPrefix(line, "pid = "))
		}
	}
	return u
}

// parseSystemctlShow extracts the pid, argv and environment names from
// `systemctl --user show -p MainPID -p ExecStart -p Environment`.
func parseSystemctlShow(out string) runningUnit {
	var u runningUnit
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "MainPID="):
			u.PID, _ = strconv.Atoi(strings.TrimPrefix(line, "MainPID="))
		case strings.HasPrefix(line, "ExecStart="):
			if _, rest, ok := strings.Cut(line, "argv[]="); ok {
				argv, _, _ := strings.Cut(rest, " ;")
				u.Argv = strings.Fields(argv)
			}
		case strings.HasPrefix(line, "Environment="):
			for _, kv := range strings.Fields(strings.TrimPrefix(line, "Environment=")) {
				if k, _, ok := strings.Cut(kv, "="); ok {
					u.EnvKeys = append(u.EnvKeys, k)
				}
			}
		}
	}
	return u
}

// processEnvKeys lists the names of the environment variables of a running process. On macOS it
// reads `ps eww` (same-user processes only), on Linux /proc/<pid>/environ. Values are discarded.
func processEnvKeys(ctx context.Context, r Runner, opts Options, pid int) []string {
	if pid <= 0 {
		return nil
	}
	var keys []string
	if opts.goos() == "darwin" {
		out, _, err := r.Run(ctx, "ps", "eww", "-p", strconv.Itoa(pid), "-o", "command=")
		if err != nil {
			return nil
		}
		// "<command line> KEY=value KEY2=value": a name is a token ending where "=" starts, and
		// the only name we look for is matched exactly, so an argument containing "=" is harmless.
		for _, tok := range strings.Fields(out) {
			if k, _, ok := strings.Cut(tok, "="); ok && k == bootstrapAdminTokenEnv {
				keys = append(keys, k)
			}
		}
		return keys
	}
	out, _, err := r.Run(ctx, "cat", "/proc/"+strconv.Itoa(pid)+"/environ")
	if err != nil {
		return nil
	}
	for _, kv := range strings.Split(out, "\x00") {
		if k, _, ok := strings.Cut(kv, "="); ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// normalizeControllerArgs turns an argv tail into a sorted set of comparable tokens:
// "--listen X", "--listen=X" and "-l X" all become "--listen=X".
func normalizeControllerArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--listen" || a == "-l":
			if i+1 < len(args) {
				i++
				out = append(out, "--listen="+args[i])
			}
		case strings.HasPrefix(a, "-l="):
			out = append(out, "--listen="+strings.TrimPrefix(a, "-l="))
		default:
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// expectedControllerUnit is what this grove would render for the controller given cfg.
func expectedControllerUnit(opts Options, cfg *config.Config) (program string, args []string, ok bool) {
	if cfg == nil || cfg.Orchard.URL == "" {
		return "", nil, false
	}
	u, err := url.Parse(normalizeURL(cfg.Orchard.URL))
	if err != nil || u.Host == "" {
		return "", nil, false
	}
	listen := hostPortOrDefault(cfg.Orchard.URL, "", defaultOrchardControllerPort)
	fleetPath := cfg.Fleet
	if fleetPath == "" {
		fleetPath = filepath.Join(opts.ConfigDir(), "fleet.yaml")
	}
	args = OrchardControllerArgs(listen, fleetNeedsHostDirs(fleetPath))
	if u.Scheme == "https" {
		// A TLS controller is the Orchard default; grove itself never installs one.
		filtered := args[:0:0]
		for _, a := range args {
			if a != "--insecure-no-tls" {
				filtered = append(filtered, a)
			}
		}
		args = filtered
	}
	return filepath.Join(orchardBinDir(opts), orchardBinName), args, true
}

// controllerUnitInstalled reports whether this host has grove's Orchard controller unit.
func controllerUnitInstalled(opts Options) bool {
	p := filepath.Join(opts.SystemdUserDir(), orchardControllerService)
	if opts.goos() == "darwin" {
		p = filepath.Join(opts.LaunchAgentsDir(), orchardControllerLabel+".plist")
	}
	_, err := os.Stat(p)
	return err == nil
}

// inspectRunningController asks the supervisor how the controller is running. ok is false when
// it is not loaded or not running (nothing to compare).
func inspectRunningController(ctx context.Context, r Runner, opts Options) (u runningUnit, ok bool) {
	if opts.goos() == "darwin" {
		out, _, err := r.Run(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), orchardControllerLabel))
		if err != nil {
			return u, false
		}
		u = parseLaunchctlPrint(out)
	} else {
		out, _, err := r.Run(ctx, "systemctl", "--user", "show", orchardControllerService,
			"-p", "MainPID", "-p", "ExecStart", "-p", "Environment")
		if err != nil {
			return u, false
		}
		u = parseSystemctlShow(out)
	}
	if u.PID <= 0 {
		return u, false
	}
	u.EnvKeys = append(u.EnvKeys, processEnvKeys(ctx, r, opts, u.PID)...)
	return u, true
}

// checkOrchardControllerDrift compares the running controller's program and flags with the unit
// this grove renders (a launchd job keeps the arguments of the plist it was bootstrapped from, so
// re-rendering the file changes nothing until a restart), and flags a bootstrap admin token in its
// environment. It returns nothing on a host without the controller unit.
func checkOrchardControllerDrift(ctx context.Context, r Runner, opts Options, cfg *config.Config) []CheckResult {
	if !controllerUnitInstalled(opts) {
		return nil
	}
	run, ok := inspectRunningController(ctx, r, opts)
	if !ok {
		return nil // not loaded: the orchard-controller reachability check says so
	}
	var results []CheckResult

	if program, want, ok := expectedControllerUnit(opts, cfg); ok {
		res := CheckResult{Name: "orchard-controller-unit"}
		if len(run.Argv) == 0 {
			res.Detail = "could not read the running controller's arguments"
			res.Warn, res.OK = true, true
		} else {
			gotArgs := normalizeControllerArgs(run.Argv[1:])
			wantArgs := normalizeControllerArgs(want)
			var diffs []string
			if run.Argv[0] != program {
				diffs = append(diffs, fmt.Sprintf("program %s, rendered unit has %s", run.Argv[0], program))
			}
			if missing := setDiff(wantArgs, gotArgs); len(missing) > 0 {
				diffs = append(diffs, "missing "+strings.Join(missing, " "))
			}
			if extra := setDiff(gotArgs, wantArgs); len(extra) > 0 {
				diffs = append(diffs, "extra "+strings.Join(extra, " "))
			}
			if len(diffs) == 0 {
				res.OK = true
				res.Detail = "running controller matches the rendered unit"
			} else {
				res.Detail = "running controller differs from the unit grove renders (" + strings.Join(diffs, "; ") + "); a restart would start the rendered one"
				res.Remediation = "follow docs/OPERATIONS.md \"Restarting the Orchard controller onto the rendered unit\""
			}
		}
		results = append(results, res)
	}

	cred := CheckResult{Name: "orchard-controller-credential", OK: true, Detail: "no " + bootstrapAdminTokenEnv + " in the running controller's environment"}
	for _, k := range run.EnvKeys {
		if k == bootstrapAdminTokenEnv {
			cred = CheckResult{
				Name:   "orchard-controller-credential",
				Detail: bootstrapAdminTokenEnv + " is set in the running controller's environment: a full-admin credential that outlives grove's own service account",
				Remediation: "restart the controller from the rendered unit (it sets no such variable), then delete the bootstrap-admin service account: " +
					"docs/OPERATIONS.md \"Deleting the bootstrap admin service account\"",
			}
			break
		}
	}
	results = append(results, cred)
	return results
}

// setDiff returns the elements of a that are not in b.
func setDiff(a, b []string) []string {
	have := make(map[string]bool, len(b))
	for _, s := range b {
		have[s] = true
	}
	var out []string
	for _, s := range a {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}

// orchardControllerDriftStep never changes anything: restarting the controller drops every
// worker's connection, so install only reports. Its Check prints what the doctor would (a running
// controller that differs from the unit just rendered, or holds a bootstrap admin token) and
// always passes.
func orchardControllerDriftStep(r Runner, opts Options, cfgDir string, out io.Writer) Step {
	return Step{
		Name:        "orchard-controller-drift",
		Description: "Report (never change) a running Orchard controller whose flags differ from the rendered unit or whose environment holds a bootstrap admin token.",
		Check: func(ctx context.Context) (bool, error) {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil || cfg == nil {
				return true, nil
			}
			for _, c := range checkOrchardControllerDrift(ctx, r, opts, cfg) {
				if !c.OK && out != nil {
					fmt.Fprintf(out, "warning: %s: %s\n  fix: %s\n", c.Name, c.Detail, c.Remediation)
				}
			}
			return true, nil
		},
		Apply: func(context.Context) error { return nil },
	}
}
