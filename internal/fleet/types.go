// Package fleet holds the desired-state model (fleet.yaml) and the reconciler that makes Orchard
// match it.
package fleet

import "time"

// Pool describes one class of worker VM to keep running on matching Orchard workers.
type Pool struct {
	Name  string `yaml:"name" json:"name"`
	Image string `yaml:"image" json:"image"`
	// PerWorker is how many VMs of this pool each matching worker should run.
	PerWorker int    `yaml:"perWorker" json:"perWorker"`
	CPU       uint64 `yaml:"cpu" json:"cpu"`
	Memory    uint64 `yaml:"memory" json:"memory"` // MiB
	DiskSize  uint64 `yaml:"disk,omitempty" json:"disk,omitempty"`
	TTL       Duration `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	// Labels are applied to created VMs in addition to pool=<name> and host=<worker>.
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// WorkerSelector restricts the pool to workers whose labels contain these entries.
	WorkerSelector map[string]string `yaml:"workerSelector,omitempty" json:"workerSelector,omitempty"`
	// Scripts run inside the guest (see ARCHITECTURE.md → Recycling). Paths or inline.
	StartupScript   string   `yaml:"startupScript,omitempty" json:"startupScript,omitempty"`
	ShutdownScript  string   `yaml:"shutdownScript,omitempty" json:"shutdownScript,omitempty"`
	ShutdownTimeout Duration `yaml:"shutdownTimeout,omitempty" json:"shutdownTimeout,omitempty"`
	Username        string   `yaml:"username,omitempty" json:"username,omitempty"`
	Password        string   `yaml:"password,omitempty" json:"password,omitempty"`
	// AllowDockerSocket opts this pool's build/agent/shell jobs into mounting the VM's docker
	// socket, which lets a dispatched job request a different container image than the fixed
	// grove-runner default (nested `docker run` — see docs/JOBS.md). Off by default: the socket
	// gives any job on the VM root-equivalent host control and breaks job-to-job isolation within
	// the pool, so this is a deliberate per-pool trade-off, not something grove enables silently.
	AllowDockerSocket bool `yaml:"allowDockerSocket,omitempty" json:"allowDockerSocket,omitempty"`
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
