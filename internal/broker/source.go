package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A placeholder is only good from the VM its job runs in. The control plane knows that VM by its
// Orchard name (Entry.VM) but not its address, which exists only on this worker's vmnet bridge. So
// the broker resolves the name itself, on each sync, and installs an entry only once it has an
// address to bind it to (Syncer.bind). An entry with no address is never installed: to Store.Lookup
// a missing Source means "refuse", never "any source".

// Runner runs a host command. It is the one method of install.Runner this package needs, kept
// local so the broker doesn't import the installer.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// SourceResolver finds the bridge addresses of this worker's running VMs.
type SourceResolver interface {
	// Sources returns the address of each VM in vms (Orchard VM names) that it could resolve, and
	// for every other one the reason it could not. A VM is in exactly one of the two maps.
	Sources(ctx context.Context, vms []string) (addrs map[string]netip.Addr, failed map[string]error)
}

// DefaultSourceTTL is how long TartSources trusts an address it resolved before asking Tart again.
const DefaultSourceTTL = 30 * time.Second

// tartTimeout bounds one tart command: a sync holds the Syncer's lock while it resolves.
const tartTimeout = 10 * time.Second

// errNoVM is why an entry that names no VM is left out.
var errNoVM = errors.New("the control plane named no VM for this job")

// TartSources resolves a VM's address the way its worker would: the worker's Orchard runs each VM
// as the local Tart VM "orchard-<name>-<uid>-<restart count>" (Orchard's ondiskname), and
// `tart ip` gives a running VM's address. It runs one `tart list` per call, and one `tart ip` per
// VM that is new, restarted (another uid or restart count) or whose address is older than TTL.
type TartSources struct {
	Runner Runner
	// Bin is the tart to run: an absolute path to the worker's Grove Tart, never a bare name that
	// PATH could resolve to another one.
	Bin string
	// TTL is how long a resolved address is trusted (DefaultSourceTTL when <= 0).
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]tartSource
	now   func() time.Time
}

type tartSource struct {
	onDisk string
	addr   netip.Addr
	at     time.Time
}

// tartVM is one row of `tart list --format json`.
type tartVM struct {
	Name    string `json:"Name"`
	Source  string `json:"Source"`
	Running bool   `json:"Running"`
}

// orchardVMName returns the Orchard VM name inside a local Tart VM's name, as Orchard's
// ondiskname.Parse reads it: "orchard-", the name (which may itself hold dashes), a five-part
// UUID and a restart count.
func orchardVMName(onDisk string) (string, bool) {
	parts := strings.Split(onDisk, "-")
	if len(parts) < 8 || parts[0] != "orchard" {
		return "", false
	}
	if _, err := strconv.ParseUint(parts[len(parts)-1], 10, 64); err != nil {
		return "", false
	}
	name := strings.Join(parts[1:len(parts)-6], "-")
	return name, name != ""
}

func (t *TartSources) run(ctx context.Context, args ...string) (string, error) {
	if t.Runner == nil || t.Bin == "" {
		return "", errors.New("no Tart to ask")
	}
	ctx, cancel := context.WithTimeout(ctx, tartTimeout)
	defer cancel()
	out, _, err := t.Runner.Run(ctx, t.Bin, args...)
	return out, err
}

// running lists this worker's running Orchard VMs: Orchard name to the local Tart names that
// carry it (more than one only while Orchard replaces a VM under the same name).
func (t *TartSources) running(ctx context.Context) (map[string][]string, error) {
	out, err := t.run(ctx, "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	var vms []tartVM
	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	byName := map[string][]string{}
	for _, vm := range vms {
		if vm.Source != "local" || !vm.Running {
			continue
		}
		if name, ok := orchardVMName(vm.Name); ok {
			byName[name] = append(byName[name], vm.Name)
		}
	}
	return byName, nil
}

func (t *TartSources) ip(ctx context.Context, onDisk string) (netip.Addr, error) {
	out, err := t.run(ctx, "ip", onDisk)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("tart ip: %w", err)
	}
	a, err := netip.ParseAddr(strings.TrimSpace(out))
	if err != nil {
		return netip.Addr{}, errors.New("tart ip: answer is not an IP address")
	}
	a = a.Unmap()
	if !a.IsPrivate() {
		return netip.Addr{}, fmt.Errorf("tart ip: %s is not a private address, so not one on the VM bridge", a)
	}
	return a, nil
}

// Sources implements SourceResolver.
func (t *TartSources) Sources(ctx context.Context, vms []string) (map[string]netip.Addr, map[string]error) {
	addrs, failed := map[string]netip.Addr{}, map[string]error{}
	want := map[string]bool{}
	for _, vm := range vms {
		if vm == "" {
			failed[vm] = errNoVM
			continue
		}
		want[vm] = true
	}
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	ttl := t.TTL
	if ttl <= 0 {
		ttl = DefaultSourceTTL
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cache == nil {
		t.cache = map[string]tartSource{}
	}
	if len(want) == 0 {
		clear(t.cache)
		return addrs, failed
	}
	fresh := func(c tartSource) bool { return now().Sub(c.at) < ttl }

	running, err := t.running(ctx)
	if err != nil {
		// Tart didn't answer. An address resolved within the TTL still stands; nothing else does.
		for vm := range want {
			if c, ok := t.cache[vm]; ok && fresh(c) {
				addrs[vm] = c.addr
			} else {
				delete(t.cache, vm)
				failed[vm] = err
			}
		}
		return addrs, failed
	}
	for vm := range t.cache {
		if !want[vm] || len(running[vm]) != 1 {
			delete(t.cache, vm)
		}
	}
	for vm := range want {
		switch onDisk := running[vm]; len(onDisk) {
		case 0:
			failed[vm] = errors.New("no running Tart VM on this worker carries that name")
		case 1:
			if c, ok := t.cache[vm]; ok && c.onDisk == onDisk[0] && fresh(c) {
				addrs[vm] = c.addr
				continue
			}
			delete(t.cache, vm)
			a, err := t.ip(ctx, onDisk[0])
			if err != nil {
				failed[vm] = err
				continue
			}
			t.cache[vm] = tartSource{onDisk: onDisk[0], addr: a, at: now()}
			addrs[vm] = a
		default:
			failed[vm] = fmt.Errorf("%d running Tart VMs on this worker carry that name", len(onDisk))
		}
	}
	// Two VMs behind one address (a DHCP lease that outlived its VM) can't be told apart by the
	// proxy, so neither gets its entries until Tart tells them apart.
	holders := map[netip.Addr][]string{}
	for vm, a := range addrs {
		holders[a] = append(holders[a], vm)
	}
	for a, vms := range holders {
		if len(vms) < 2 {
			continue
		}
		sort.Strings(vms)
		for _, vm := range vms {
			delete(addrs, vm)
			delete(t.cache, vm)
			failed[vm] = fmt.Errorf("address %s is also the address of another VM here (%s)", a, strings.Join(vms, ", "))
		}
	}
	return addrs, failed
}
