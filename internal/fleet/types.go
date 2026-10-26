// Package fleet holds the desired-state model (fleet.yaml) and the reconciler that makes Orchard
// match it.
package fleet

import (
	"strconv"
	"strings"
	"time"
)

// Pool describes one class of worker VM to keep running on matching Orchard workers.
type Pool struct {
	Name  string `yaml:"name" json:"name"`
	Image string `yaml:"image" json:"image"`
	// PerWorker is how many VMs of this pool each matching worker should run.
	PerWorker int      `yaml:"perWorker" json:"perWorker"`
	CPU       uint64   `yaml:"cpu" json:"cpu"`
	Memory    uint64   `yaml:"memory" json:"memory"` // MiB
	DiskSize  uint64   `yaml:"disk,omitempty" json:"disk,omitempty"`
	TTL       Duration `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	// Labels are sent to Orchard as the created VM's labels, in addition to the
	// org.cirruslabs.orchard.worker-name pin grove always sets (see internal/orchard/client.go
	// vmToV1). Orchard's scheduler treats a VM's labels as a hard selector — it only ever places
	// a VM on a worker whose own labels are a *superset* of the VM's (see
	// internal/controller/scheduler.schedulingLoopIteration in the gm2211/orchard fork) — so
	// every key here must also be present, with the same value, on any worker meant to run this
	// pool (pass it via `orchard worker run --labels k=v`). A label that no worker carries means
	// VMs of this pool sit "pending" forever, since no worker ever satisfies the selector. This
	// is unrelated to grove's own pool/host bookkeeping: a VM's pool and host are derived from
	// its name and observed placement (see ParseVMName), never labelled.
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// WorkerSelector restricts the pool to workers whose labels contain these entries. Unlike
	// Labels above, this is evaluated entirely by grove itself while planning (internal/fleet
	// Reconciler.Plan) and never reaches Orchard.
	WorkerSelector map[string]string `yaml:"workerSelector,omitempty" json:"workerSelector,omitempty"`
	// Scripts run inside the guest (see ARCHITECTURE.md → Recycling). Paths or inline.
	StartupScript   string   `yaml:"startupScript,omitempty" json:"startupScript,omitempty"`
	ShutdownScript  string   `yaml:"shutdownScript,omitempty" json:"shutdownScript,omitempty"`
	ShutdownTimeout Duration `yaml:"shutdownTimeout,omitempty" json:"shutdownTimeout,omitempty"`
	Username        string   `yaml:"username,omitempty" json:"username,omitempty"`
	Password        string   `yaml:"password,omitempty" json:"password,omitempty"`
	// AllowDockerSocket is no longer supported and only kept so Load can refuse it by name: the
	// socket gave every job root on the guest, past the fence that keeps jobs off home networks.
	AllowDockerSocket bool `yaml:"allowDockerSocket,omitempty" json:"allowDockerSocket,omitempty"`
	// Network chooses what this pool's VMs can reach. Empty or "shared" keeps Tart's default
	// shared NAT, where a guest reaches everything its host Mac reaches (though its jobs never do:
	// see homeFenceScript): the LAN, the host's own
	// services, and the whole tailnet through the host. "isolated" runs each VM on Softnet with
	// the host blocked, so the guest reaches only public addresses, and gives it its own tagged
	// tailnet login for the one control-plane port it needs (see isolation.go and
	// docs/OPERATIONS.md "Isolating a pool's network").
	Network string `yaml:"network,omitempty" json:"network,omitempty"`
	// RunnerImage selects the fixed outer container used by every Linux Nomad job in this pool.
	// It is independent of the VM image above and opt-in per-dispatch nested Docker overrides.
	RunnerImage string `yaml:"runnerImage,omitempty" json:"runnerImage,omitempty"`
	// HostXcode shares one host-installed Xcode application read-only with guests in this pool.
	// It is intentionally limited to the macos pool and pinned to the host app's build version.
	HostXcode *HostXcodeConfig `yaml:"hostXcode,omitempty" json:"hostXcode,omitempty"`
	// JobCPU/JobMemory are this pool's per-JOB Nomad resource defaults (MHz / MiB) — they size the
	// `resources` block of every build/agent/shell job dispatched against this pool (see
	// dispatch.PoolConfig, threaded through by internal/cli/serve.go's poolConfigs). These are
	// deliberately distinct from CPU/Memory above, which size the pool's Orchard VMs themselves
	// (the VM the job then runs inside) — a job's resources request is a slice of that VM's
	// capacity, not the same number. Zero means "use the built-in default" — see
	// JobCPUOrDefault/JobMemoryOrDefault.
	JobCPU    uint64 `yaml:"jobCPU,omitempty" json:"jobCPU,omitempty"`
	JobMemory uint64 `yaml:"jobMemory,omitempty" json:"jobMemory,omitempty"`
	// Fallback names the pool this pool's jobs move to when they can't be placed here (see
	// Spec.FallbackFor and docs/JOBS.md "Falling back to macOS"). Empty means automatic: a shared
	// Linux pool falls back to a shared macos pool, since most Linux build scripts run on macOS
	// too. FallbackNone turns it off.
	Fallback string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	// MinPerWorker and MaxPerWorker make a shared pool elastic: each matching worker runs between
	// min and max of its VMs, more while jobs wait for the pool, fewer once they sit idle, and a
	// full worker swaps an idle VM of another elastic pool for one of this pool (see
	// autoscale.go). Setting MaxPerWorker turns this on; MinPerWorker then defaults to 0 and
	// PerWorker is ignored. Unset, the pool runs exactly PerWorker VMs per worker, as always.
	MinPerWorker *int `yaml:"minPerWorker,omitempty" json:"minPerWorker,omitempty"`
	MaxPerWorker *int `yaml:"maxPerWorker,omitempty" json:"maxPerWorker,omitempty"`
}

// Bounds is how many of p's VMs each matching worker runs: at least min, at most max.
func (p Pool) Bounds() (min, max int) {
	if p.MaxPerWorker == nil {
		return p.PerWorker, p.PerWorker
	}
	if p.MinPerWorker != nil {
		min = *p.MinPerWorker
	}
	return min, *p.MaxPerWorker
}

// Elastic reports whether p's VM count follows demand (see Bounds).
func (p Pool) Elastic() bool {
	min, max := p.Bounds()
	return min != max
}

// FallbackNone is the Pool.Fallback value that keeps a pool's jobs in that pool.
const FallbackNone = "none"

// FallbackFor resolves p.Fallback against the spec: the pool p's unplaceable jobs move to, or ""
// for none. Only a shared pool falls back, and only to another shared pool: a sealed pool's jobs
// run under different isolation and on the control plane's own Nomad client.
func (s *Spec) FallbackFor(p Pool) string {
	if p.Isolated() || p.Fallback == FallbackNone {
		return ""
	}
	target := p.Fallback
	if target == "" {
		if p.MacOS() {
			return ""
		}
		target = MacOSPool
	}
	for _, q := range s.Pools {
		if q.Name == target && q.Name != p.Name && !q.Isolated() {
			return target
		}
	}
	return ""
}

// MacOSPool is the name of the pool whose VMs are macOS guests; every other pool is Linux.
const MacOSPool = "macos"

// MacOS reports whether p's VMs are macOS guests (the macos pool).
func (p Pool) MacOS() bool { return p.Name == MacOSPool }

// HostXcodeConfig describes an Xcode application that Orchard mounts from the host into a
// macOS guest at /Volumes/My Shared Files/grove-xcode.app.
type HostXcodeConfig struct {
	Path         string `yaml:"path" json:"path"`
	BuildVersion string `yaml:"buildVersion" json:"buildVersion"`
}

// Built-in per-job Nomad resource defaults (MHz / MiB) used when a pool doesn't set
// jobCPU/jobMemory in fleet.yaml. Deliberately modest — enough for a typical CI/agent script
// without reserving so much of a pool VM's capacity that few jobs can run concurrently on it.
const (
	DefaultJobCPU    uint64 = 500  // MHz
	DefaultJobMemory uint64 = 1024 // MiB
)

// JobCPUOrDefault returns p.JobCPU if set, else DefaultJobCPU.
func (p Pool) JobCPUOrDefault() uint64 {
	if p.JobCPU > 0 {
		return p.JobCPU
	}
	return DefaultJobCPU
}

// JobMemoryOrDefault returns p.JobMemory if set, else DefaultJobMemory.
func (p Pool) JobMemoryOrDefault() uint64 {
	if p.JobMemory > 0 {
		return p.JobMemory
	}
	return DefaultJobMemory
}

// Spec is the whole fleet.yaml.
type Spec struct {
	Pools []Pool `yaml:"pools" json:"pools"`
}

// Duration is a time.Duration that (un)marshals as "12h" in YAML/JSON.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// VMName is the deterministic name of the n-th VM of a pool on a worker.
func VMName(pool, worker string, n int) string {
	return pool + "-" + worker + "-" + itoa(n)
}

// ParseVMName is VMName's inverse: given a VM name grove could have generated, it recovers pool,
// worker, and n. It relies on pool names being restricted to a dash/dot-free alphabet (see
// Spec.Validate) so the first "-" unambiguously ends the pool component, while worker names may
// themselves contain dashes or dots (real-world hostnames like "my-macbook-pro.local"), so the
// *last* "-" is taken to end the worker component and start the trailing index.
//
// ok is false for anything that doesn't parse as "<pool>-<worker>-<n>" — including plain names
// with no "-" at all, or a worker component sitting between two adjacent "-"s that turns out to
// be empty.
func ParseVMName(name string) (pool, worker string, n int, ok bool) {
	i := strings.IndexByte(name, '-')
	if i < 0 || i == len(name)-1 {
		return "", "", 0, false
	}
	pool = name[:i]

	rest := name[i+1:]
	j := strings.LastIndexByte(rest, '-')
	if j <= 0 || j == len(rest)-1 {
		return "", "", 0, false
	}
	worker = rest[:j]

	n, err := strconv.Atoi(rest[j+1:])
	if err != nil {
		return "", "", 0, false
	}

	return pool, worker, n, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
