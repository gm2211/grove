package install

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/wire"
)

// httpCheckTimeout bounds every doctor HTTP probe so `grove doctor` never hangs waiting on an
// unreachable host.
const httpCheckTimeout = 3 * time.Second

// maxMacOSVMSlots is Apple's Virtualization.framework cap on concurrent macOS guests per host.
const maxMacOSVMSlots = 2

// minTartDiskFreeGiB is the free-space threshold under ~/.tart doctor warns below.
const minTartDiskFreeGiB = 50.0

// minMacOSCPUTotalCompute is the threshold below which a macOS Nomad client's fingerprinted
// cpu.totalcompute (Node.CPUMHz) is almost certainly the broken stock Apple Silicon fingerprint
// (observed as low as 24 on a real M5 Max) rather than a real MHz total — see
// docs/OPERATIONS.md "jobs pending with DimensionExhausted cpu on macOS". Any believable
// per-core-scaled override (internal/fleet/scripts.go: ncpu * 2000) lands far above this.
const minMacOSCPUTotalCompute = 1000

// macOSCPUFingerprintCheckTimeout bounds checkMacOSCPUFingerprint's handful of Nomad API calls
// (ListNodes does one List + one Info + one Allocations call per node), independent of the
// single-call httpCheckTimeout used by checkHTTPEndpoint.
const macOSCPUFingerprintCheckTimeout = 3 * httpCheckTimeout

// CheckResult is one row of `grove doctor`'s table.
type CheckResult struct {
	Name        string
	OK          bool
	Detail      string
	Remediation string // shown only when !OK
}

// RunDoctor runs every check and returns them in table order. cfg may be nil if no config.yaml
// exists yet. It never blocks longer than a few seconds total: every network call carries its own
// timeout and errors turn into a failed CheckResult rather than propagating.
func RunDoctor(ctx context.Context, r Runner, opts Options, cfg *config.Config) []CheckResult {
	var results []CheckResult

	results = append(results, checkTailscale(ctx, opts))

	if cfg == nil {
		results = append(results, CheckResult{
			Name:        "config",
			OK:          false,
			Detail:      "no config.yaml found",
			Remediation: "run `grove install --role worker|control-plane|client`",
		})
	} else {
		results = append(results,
			checkHTTPEndpoint(ctx, "orchard-controller", cfg.Orchard.URL, "/v1/controller/info", cfg.Orchard.Token),
			checkHTTPEndpoint(ctx, "nomad", cfg.Nomad.URL, "/v1/agent/self", ""),
			checkHTTPEndpoint(ctx, "grove-server", cfg.Server.URL, "/api/v1/healthz", cfg.Server.Token),
		)
		results = append(results, checkMacOSCPUFingerprint(ctx, cfg))
	}

	results = append(results, checkServiceUnits(opts))
	results = append(results, checkTart(ctx, r, opts)...)

	return results
}

func checkTailscale(ctx context.Context, opts Options) CheckResult {
	bin, err := FindTailscale(opts.lookPath(), opts.exists())
	if err != nil {
		return CheckResult{
			Name:        "tailscale",
			OK:          false,
			Detail:      err.Error(),
			Remediation: "install the standalone Tailscale client and run `tailscale up`",
		}
	}
	st, err := GetTailscaleStatus(ctx, ExecRunner{}, bin)
	if err != nil {
		return CheckResult{Name: "tailscale", OK: false, Detail: err.Error(), Remediation: "run `tailscale up`"}
	}
	if st.BackendState != "Running" {
		return CheckResult{
			Name:        "tailscale",
			OK:          false,
			Detail:      "backend state: " + st.BackendState,
			Remediation: "run `tailscale up`",
		}
	}
	return CheckResult{Name: "tailscale", OK: true, Detail: fmt.Sprintf("%s (%s)", st.TailnetIP(), st.Self.DNSName)}
}

// checkHTTPEndpoint GETs base+path with a bounded timeout and an optional bearer token, treating
// any 2xx as healthy. base=="" is reported as not-configured rather than attempted.
func checkHTTPEndpoint(ctx context.Context, name, base, path, token string) CheckResult {
	if base == "" {
		return CheckResult{
			Name:        name,
			OK:          false,
			Detail:      "not configured",
			Remediation: "run `grove install` to set it, or add it to config.yaml",
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, httpCheckTimeout)
	defer cancel()

	url := strings.TrimRight(base, "/") + path
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error()}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: httpCheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{
			Name:        name,
			OK:          false,
			Detail:      fmt.Sprintf("GET %s: %v", url, err),
			Remediation: "check the service is running and reachable over the tailnet",
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CheckResult{
			Name:        name,
			OK:          false,
			Detail:      fmt.Sprintf("GET %s: HTTP %d", url, resp.StatusCode),
			Remediation: "check the service logs",
		}
	}
	return CheckResult{Name: name, OK: true, Detail: fmt.Sprintf("GET %s: HTTP %d", url, resp.StatusCode)}
}

// checkMacOSCPUFingerprint warns when any macOS Nomad client node in the cluster is fingerprinting
// an implausibly low cpu.totalcompute (Node.CPUMHz) — the Apple Silicon fingerprinter bug that
// makes every job placement fail with DimensionExhausted cpu. internal/fleet/scripts.go's
// StartupScript is supposed to override this at boot (see nomadMetaScript's cpu_total_compute),
// so seeing it this low means that override either didn't run or didn't take effect (e.g. the
// Nomad client wasn't restarted after grove-meta.hcl was written).
//
// This is best-effort: any error reaching Nomad is reported as OK/skipped rather than failing the
// check outright, since the "nomad" checkHTTPEndpoint check above already reports Nomad
// reachability — this check would just be noise duplicating that failure.
func checkMacOSCPUFingerprint(ctx context.Context, cfg *config.Config) CheckResult {
	const name = "macos-cpu-fingerprint"
	if cfg == nil || cfg.Nomad.URL == "" {
		return CheckResult{Name: name, OK: true, Detail: "nomad not configured (skipped)"}
	}

	nc, err := wire.NewNomadClient(cfg.Nomad)
	if err != nil {
		return CheckResult{Name: name, OK: true, Detail: "could not build nomad client (skipped): " + err.Error()}
	}

	reqCtx, cancel := context.WithTimeout(ctx, macOSCPUFingerprintCheckTimeout)
	defer cancel()
	nodes, err := nc.ListNodes(reqCtx)
	if err != nil {
		return CheckResult{Name: name, OK: true, Detail: "could not list nomad nodes (skipped): " + err.Error()}
	}

	var low []string
	for _, n := range nodes {
		if n.NodeClass != "macos" {
			continue
		}
		if n.CPUMHz > 0 && n.CPUMHz < minMacOSCPUTotalCompute {
			low = append(low, fmt.Sprintf("%s(cpu.totalcompute=%d)", n.Name, n.CPUMHz))
		}
	}
	if len(low) > 0 {
		return CheckResult{
			Name:        name,
			OK:          false,
			Detail:      "implausibly low CPU fingerprint on: " + strings.Join(low, ", "),
			Remediation: "jobs will fail to place with DimensionExhausted cpu — see docs/OPERATIONS.md \"jobs pending with DimensionExhausted cpu on macOS\" (set cpu_total_compute in grove-meta.hcl and restart the node's Nomad client)",
		}
	}
	return CheckResult{Name: name, OK: true, Detail: "no undersized macOS CPU fingerprints found"}
}

// checkServiceUnits reports whether grove's launchd agents (macOS) or systemd --user units
// (Linux) are present on disk. It doesn't require knowing the role: it just lists what it finds.
func checkServiceUnits(opts Options) CheckResult {
	var dir, pattern string
	if opts.goos() == "darwin" {
		dir = opts.LaunchAgentsDir()
		pattern = "com.grove.*.plist"
	} else {
		dir = opts.SystemdUserDir()
		pattern = "grove-*.service"
	}
	matches, _ := filepath.Glob(filepath.Join(dir, pattern))
	if len(matches) == 0 {
		return CheckResult{
			Name:        "service-units",
			OK:          false,
			Detail:      fmt.Sprintf("none found under %s", dir),
			Remediation: "run `grove install --role worker|control-plane`",
		}
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = filepath.Base(m)
	}
	return CheckResult{Name: "service-units", OK: true, Detail: strings.Join(names, ", ")}
}

// checkTart reports whether Tart is installed and, if so, how many macOS VM slots (grove's
// "macos-*" pool VMs) are currently running against Apple's 2-VM-per-host cap.
func checkTart(ctx context.Context, r Runner, opts Options) []CheckResult {
	if _, err := opts.lookPath()("tart"); err != nil {
		return []CheckResult{{
			Name:        "tart",
			OK:          false,
			Detail:      "not installed",
			Remediation: "run `grove install --role worker` (or `brew install openai/tools/tart`)",
		}}
	}
	results := []CheckResult{{Name: "tart", OK: true, Detail: "installed"}}

	stdout, _, err := r.Run(ctx, "tart", "list", "--format", "json")
	if err != nil {
		results = append(results, CheckResult{
			Name:        "tart-vm-slots",
			OK:          false,
			Detail:      fmt.Sprintf("`tart list` failed: %v", err),
			Remediation: "run `tart list` manually to see what's wrong",
		})
		return append(results, checkTartDiskFree(opts))
	}
	running, err := countRunningMacOSVMs(stdout)
	if err != nil {
		results = append(results, CheckResult{Name: "tart-vm-slots", OK: false, Detail: err.Error()})
	} else {
		results = append(results, CheckResult{
			Name:        "tart-vm-slots",
			OK:          running <= maxMacOSVMSlots,
			Detail:      fmt.Sprintf("%d/%d macOS VM slots used", running, maxMacOSVMSlots),
			Remediation: "recycle or delete a macOS VM: `grove` fleet reconciler will recreate it",
		})
	}
	return append(results, checkTartDiskFree(opts))
}

// tartVM mirrors Tart's `tart list --format json` VMInfo struct.
type tartVM struct {
	Source  string `json:"Source"`
	Name    string `json:"Name"`
	Running bool   `json:"Running"`
	State   string `json:"State"`
}

func countRunningMacOSVMs(jsonOutput string) (int, error) {
	var vms []tartVM
	if err := json.Unmarshal([]byte(jsonOutput), &vms); err != nil {
		return 0, fmt.Errorf("parse tart list output: %w", err)
	}
	n := 0
	for _, vm := range vms {
		if vm.Running && strings.HasPrefix(vm.Name, "macos-") {
			n++
		}
	}
	return n, nil
}

func checkTartDiskFree(opts Options) CheckResult {
	dir := filepath.Join(opts.homeDir(), ".tart")
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return CheckResult{Name: "tart-disk-free", OK: true, Detail: fmt.Sprintf("could not stat %s: %v (skipped)", dir, err)}
	}
	freeGiB := float64(uint64(stat.Bavail)*uint64(stat.Bsize)) / (1 << 30)
	ok := freeGiB >= minTartDiskFreeGiB
	r := CheckResult{Name: "tart-disk-free", OK: ok, Detail: fmt.Sprintf("%.1f GiB free under %s", freeGiB, dir)}
	if !ok {
		r.Remediation = "delete unused images/VMs: `tart list`, `tart delete <name>`"
	}
	return r
}
