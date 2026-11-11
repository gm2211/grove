package broker

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const testTart = "/Users/ci/.config/grove/tart-bin/tart"

// fakeTart is a Runner that answers `tart list --format json` and `tart ip <vm>` from tables.
type fakeTart struct {
	t       *testing.T
	list    string
	listErr error
	ips     map[string]string
	calls   []string
}

func (f *fakeTart) Run(_ context.Context, name string, args ...string) (string, string, error) {
	if name != testTart {
		f.t.Errorf("ran %q, want only the configured tart", name)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) == 3 && args[0] == "list" && args[1] == "--format" && args[2] == "json":
		return f.list, "", f.listErr
	case len(args) == 2 && args[0] == "ip":
		if ip, ok := f.ips[args[1]]; ok {
			return ip + "\n", "", nil
		}
		return "", "no IP address found", errors.New("exit status 1")
	}
	f.t.Errorf("unexpected tart %v", args)
	return "", "", errors.New("unexpected")
}

func (f *fakeTart) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const (
	uidA = "0b1e5a3c-1f0e-4c56-9d2a-6f3c2b1a0d9e"
	uidB = "7c9d2e4f-aaaa-4bbb-8ccc-123456789abc"
)

func tartList(rows ...string) string { return "[" + strings.Join(rows, ",") + "]" }

func tartRow(name string, running bool) string {
	r := "false"
	if running {
		r = "true"
	}
	return `{"Name":"` + name + `","Source":"local","Running":` + r + `,"State":"running","Disk":50}`
}

func TestOrchardVMName(t *testing.T) {
	for onDisk, want := range map[string]string{
		"orchard-vm1-" + uidA + "-0":                   "vm1",
		"orchard-linux-shared-mini-01-" + uidA + "-3":  "linux-shared-mini-01",
		"orchard-a-b-" + uidA + "-12":                  "a-b",
		"orchard-" + uidA + "-0":                       "", // no name
		"vm1-" + uidA + "-0":                           "",
		"orchardx-vm1-" + uidA + "-0":                  "",
		"orchard-vm1-" + uidA + "-x":                   "",
		"orchard-vm1":                                  "",
		"ghcr.io/cirruslabs/macos-sequoia-base:latest": "",
	} {
		got, ok := orchardVMName(onDisk)
		if got != want || ok != (want != "") {
			t.Errorf("orchardVMName(%q) = %q, %v; want %q", onDisk, got, ok, want)
		}
	}
}

func TestTartSources(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	a0, b0 := "orchard-vm-a-"+uidA+"-0", "orchard-vm-b-"+uidB+"-0"
	tart := &fakeTart{
		t: t,
		list: tartList(
			tartRow(a0, true),
			tartRow(b0, true),
			tartRow("orchard-vm-stopped-"+uidA+"-0", false),
			tartRow("someones-own-vm", true),
			`{"Name":"ghcr.io/cirruslabs/ubuntu:latest","Source":"OCI","Running":false}`,
			// An OCI image can't run; were one ever listed as running, its name still isn't a VM here.
			`{"Name":"orchard-vm-oci-`+uidA+`-0","Source":"OCI","Running":true}`,
		),
		ips: map[string]string{a0: "192.168.64.5", b0: "192.168.64.6"},
	}
	src := &TartSources{Runner: tart, Bin: testTart, now: func() time.Time { return now }}
	ctx := context.Background()

	addrs, failed := src.Sources(ctx, []string{"vm-a", "vm-b", "vm-stopped", "vm-gone", "vm-oci", "someones-own-vm", ""})
	if len(addrs) != 2 || addrs["vm-a"] != netip.MustParseAddr("192.168.64.5") || addrs["vm-b"] != netip.MustParseAddr("192.168.64.6") {
		t.Fatalf("addrs = %v", addrs)
	}
	for _, vm := range []string{"vm-stopped", "vm-gone", "vm-oci", "someones-own-vm", ""} {
		if failed[vm] == nil {
			t.Fatalf("%q: no failure, want it unresolved (addrs %v)", vm, addrs)
		}
	}
	if !errors.Is(failed[""], errNoVM) {
		t.Fatalf("empty name: %v", failed[""])
	}
	if tart.count("list") != 1 || tart.count("ip") != 2 {
		t.Fatalf("calls = %q, want one list and an ip for each running VM asked for", tart.calls)
	}
	// The VM's on-disk name comes from Tart's own list, never from the name the control plane sent.
	for _, c := range tart.calls {
		if strings.HasPrefix(c, "ip ") && c != "ip "+a0 && c != "ip "+b0 {
			t.Fatalf("tart %s", c)
		}
	}

	// Within the TTL a sync costs one `tart list` and no `tart ip`.
	now = now.Add(DefaultSourceTTL - time.Second)
	if addrs, _ = src.Sources(ctx, []string{"vm-a", "vm-b"}); len(addrs) != 2 || tart.count("list") != 2 || tart.count("ip") != 2 {
		t.Fatalf("cached: addrs %v, calls %q", addrs, tart.calls)
	}

	// Past the TTL the address is asked for again.
	now = now.Add(2 * time.Second)
	tart.ips[a0] = "192.168.64.50"
	if addrs, _ = src.Sources(ctx, []string{"vm-a"}); addrs["vm-a"] != netip.MustParseAddr("192.168.64.50") || tart.count("ip") != 3 {
		t.Fatalf("after TTL: addrs %v, calls %q", addrs, tart.calls)
	}

	// A restarted VM (Orchard bumps the restart count; a replaced one has another uid) is resolved
	// again at once, TTL or not, and the stopped instance's address is never used.
	a1 := "orchard-vm-a-" + uidA + "-1"
	tart.list = tartList(tartRow(a0, false), tartRow(a1, true))
	tart.ips[a1] = "192.168.64.7"
	if addrs, _ = src.Sources(ctx, []string{"vm-a"}); addrs["vm-a"] != netip.MustParseAddr("192.168.64.7") {
		t.Fatalf("after restart: addrs %v, calls %q", addrs, tart.calls)
	}

	// Restarted but no lease yet: unresolved, not the old address; resolved on a later call.
	a2 := "orchard-vm-a-" + uidA + "-2"
	tart.list = tartList(tartRow(a2, true))
	if addrs, failed = src.Sources(ctx, []string{"vm-a"}); len(addrs) != 0 || failed["vm-a"] == nil {
		t.Fatalf("no lease yet: addrs %v, failed %v", addrs, failed)
	}
	tart.ips[a2] = "192.168.64.8"
	if addrs, _ = src.Sources(ctx, []string{"vm-a"}); addrs["vm-a"] != netip.MustParseAddr("192.168.64.8") {
		t.Fatalf("lease arrived: addrs %v", addrs)
	}

	// Tart not answering: an address resolved within the TTL stands, and then nothing does.
	tart.listErr = errors.New("exit status 1")
	if addrs, failed = src.Sources(ctx, []string{"vm-a", "vm-b"}); addrs["vm-a"] != netip.MustParseAddr("192.168.64.8") || failed["vm-b"] == nil {
		t.Fatalf("list failing, within TTL: addrs %v, failed %v", addrs, failed)
	}
	now = now.Add(DefaultSourceTTL)
	if addrs, failed = src.Sources(ctx, []string{"vm-a"}); len(addrs) != 0 || failed["vm-a"] == nil {
		t.Fatalf("list failing, past TTL: addrs %v, failed %v", addrs, failed)
	}
}

func TestTartSourcesRefuses(t *testing.T) {
	a0, a1, b0 := "orchard-vm-a-"+uidA+"-0", "orchard-vm-a-"+uidB+"-0", "orchard-vm-b-"+uidB+"-0"
	ctx := context.Background()
	for name, tc := range map[string]struct {
		list string
		ips  map[string]string
		vms  []string
	}{
		"two running VMs under one name": {tartList(tartRow(a0, true), tartRow(a1, true)), map[string]string{a0: "192.168.64.5", a1: "192.168.64.6"}, []string{"vm-a"}},
		"two VMs behind one address":     {tartList(tartRow(a0, true), tartRow(b0, true)), map[string]string{a0: "192.168.64.5", b0: "192.168.64.5"}, []string{"vm-a", "vm-b"}},
		"an answer that is no address":   {tartList(tartRow(a0, true)), map[string]string{a0: "192.168.64.5 extra"}, []string{"vm-a"}},
		"an empty answer":                {tartList(tartRow(a0, true)), map[string]string{a0: ""}, []string{"vm-a"}},
		"a public address":               {tartList(tartRow(a0, true)), map[string]string{a0: "140.82.112.6"}, []string{"vm-a"}},
		"loopback":                       {tartList(tartRow(a0, true)), map[string]string{a0: "127.0.0.1"}, []string{"vm-a"}},
		"a list that is not JSON":        {"Source Name\nlocal " + a0, map[string]string{a0: "192.168.64.5"}, []string{"vm-a"}},
	} {
		src := &TartSources{Runner: &fakeTart{t: t, list: tc.list, ips: tc.ips}, Bin: testTart}
		addrs, failed := src.Sources(ctx, tc.vms)
		if len(addrs) != 0 || len(failed) != len(tc.vms) {
			t.Errorf("%s: addrs %v, failed %v; want every VM unresolved", name, addrs, failed)
		}
	}

	// No tart configured, or no runner: nothing resolves, and nothing is run.
	tart := &fakeTart{t: t, list: tartList(tartRow(a0, true)), ips: map[string]string{a0: "192.168.64.5"}}
	for _, src := range []*TartSources{{Runner: tart}, {Bin: testTart}} {
		if addrs, failed := src.Sources(ctx, []string{"vm-a"}); len(addrs) != 0 || failed["vm-a"] == nil {
			t.Errorf("unconfigured: addrs %v, failed %v", addrs, failed)
		}
	}
	if len(tart.calls) != 0 {
		t.Errorf("ran tart with no path to it: %q", tart.calls)
	}

	// Nothing to resolve: no command at all.
	src := &TartSources{Runner: tart, Bin: testTart}
	if addrs, failed := src.Sources(ctx, []string{""}); len(addrs) != 0 || !errors.Is(failed[""], errNoVM) || len(tart.calls) != 0 {
		t.Errorf("only the empty name: addrs %v, failed %v, calls %q", addrs, failed, tart.calls)
	}
}
