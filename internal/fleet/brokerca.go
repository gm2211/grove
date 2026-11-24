package fleet

import (
	"fmt"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// BrokerCAs remembers, per worker, when the credential broker on it made the CA it serves now.
// A shared VM fetches that CA once, at boot (brokerScript), and trusts only it. A broker that
// replaces its CA (LoadOrCreateCA does when a release adds an inject host) leaves every VM booted
// before then trusting the old one, and each clone or credential call through the broker fails
// with "self signed certificate in certificate chain" until the VM is rebuilt. Plan rebuilds such
// a VM by itself once it is idle (staleBrokerCA), so nobody has to recycle VMs after an upgrade.
//
// The worker's broker reports the time on each sync (GET /broker/entries, a header; see
// internal/server); only a broker credential bound to its machine is listened to. In memory only:
// after a restart the next sync, seconds later, reports it again. The zero value is ready; a nil
// *BrokerCAs knows nothing.
type BrokerCAs struct {
	mu    sync.Mutex
	since map[string]time.Time
}

// brokerCASkew is how far ahead of the control plane's clock a reported time may be. A time
// further in the future is ignored: every VM would look older than it, and be rebuilt over and
// over.
const brokerCASkew = 5 * time.Minute

// Observe records that worker's broker made its CA at made, as seen at now. It reports whether
// that changed what was known. A zero time or one more than brokerCASkew ahead of now is ignored.
func (b *BrokerCAs) Observe(worker string, made, now time.Time) bool {
	if b == nil || worker == "" || made.IsZero() || made.After(now.Add(brokerCASkew)) {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.since == nil {
		b.since = map[string]time.Time{}
	}
	if old, ok := b.since[worker]; ok && old.Equal(made) {
		return false
	}
	b.since[worker] = made
	return true
}

// Since returns when worker's broker made the CA it serves, if its broker has said.
func (b *BrokerCAs) Since(worker string) (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.since[worker]
	return t, ok
}

// staleBrokerCA says why vm, a VM of the shared pool on worker, must be rebuilt because it booted
// before its worker's broker made the CA it serves now, or "" when it needn't. A VM running a job
// (JobLoad.Busy), or on a tick that couldn't read the load, is left until a tick sees it idle; a
// sealed pool's VMs have no broker.
func (r *Reconciler) staleBrokerCA(pool Pool, worker string, vm orchard.VM) string {
	if pool.Isolated() || vm.CreatedAt.IsZero() {
		return ""
	}
	made, ok := r.Options.BrokerCAs.Since(worker)
	if !ok || !vm.CreatedAt.Before(made) {
		return ""
	}
	if r.Options.Load == nil || r.Options.Load.Busy[vm.Name] {
		return "" // busy, or the tick couldn't tell: never delete a VM under its job
	}
	return fmt.Sprintf("trusts the credential broker's previous CA: booted %s, the broker on %s made its CA %s",
		vm.CreatedAt.UTC().Format(time.RFC3339), worker, made.UTC().Format(time.RFC3339))
}
