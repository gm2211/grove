package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// fakeSources is a SourceResolver over a fixed table: a VM in addrs resolves, any other fails.
type fakeSources struct {
	addrs map[string]netip.Addr
	calls int
}

func (f *fakeSources) Sources(_ context.Context, vms []string) (map[string]netip.Addr, map[string]error) {
	f.calls++
	addrs, failed := map[string]netip.Addr{}, map[string]error{}
	for _, vm := range vms {
		if a, ok := f.addrs[vm]; ok {
			addrs[vm] = a
		} else {
			failed[vm] = errors.New("no running Tart VM on this worker carries that name")
		}
	}
	return addrs, failed
}

func TestSyncer(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	e := newEntry(t, "j", KindGitHub, "t")
	e.Expires = now.Add(Lease)
	e.VM = "vm-1"
	e.Source = netip.Addr{} // as the control plane sends it
	var fetches int
	var next []Entry
	var fail error
	store := NewStore(0)
	store.now = func() time.Time { return now }
	s := &Syncer{
		Store:   store,
		Sources: &fakeSources{addrs: map[string]netip.Addr{"vm-1": netip.MustParseAddr("192.168.64.5")}},
		Fetch: func(context.Context) ([]Entry, error) {
			fetches++
			return next, fail
		},
		now: func() time.Time { return now },
	}
	ctx := context.Background()

	if s.Synced() {
		t.Fatal("Synced before any sync")
	}
	next = []Entry{e}
	s.Sync(ctx)
	if store.Len() != 1 || fetches != 1 || !s.Synced() {
		t.Fatalf("Len %d, fetches %d, synced %v", store.Len(), fetches, s.Synced())
	}
	// A job asking about unknown placeholders in a loop costs at most one fetch per two seconds.
	for i := 0; i < 10; i++ {
		s.Sync(ctx)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want rate-limited to 1", fetches)
	}

	// Control plane unreachable: entries stay until their lease ends, then go.
	now = now.Add(time.Minute)
	fail = errors.New("down")
	s.Sync(ctx)
	if store.Len() != 1 {
		t.Fatal("failed sync dropped live entries")
	}
	if s.Synced() {
		t.Fatal("Synced after a failed sync")
	}
	now = now.Add(Lease)
	s.Sync(ctx)
	if store.Len() != 0 {
		t.Fatal("entry outlived its lease while the control plane was down")
	}

	// Control plane back, job finished: the list comes back empty and so does the store.
	fail = nil
	e.Expires = now.Add(Lease)
	next = []Entry{e}
	now = now.Add(time.Minute)
	s.Sync(ctx)
	next = nil
	now = now.Add(time.Minute)
	s.Sync(ctx)
	if store.Len() != 0 {
		t.Fatal("finished job's entry kept")
	}
}

// The fix for spec S8's open clause: an entry is installed only bound to its VM's address on this
// worker, and one that can't be bound is left out, never installed for "any source".
func TestSyncerBindsEntriesToTheirVM(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	vm1, vm2 := netip.MustParseAddr("192.168.64.5"), netip.MustParseAddr("192.168.64.6")
	entry := func(job, vm, token string) Entry {
		e := newEntry(t, job, KindGitHub, token)
		e.Expires = now.Add(Lease)
		e.VM = vm
		e.Source = netip.Addr{}
		return e
	}
	a, b := entry("job-a", "vm-1", "ghs_TOKENA"), entry("job-b", "vm-2", "ghs_TOKENB")
	noVM := entry("job-c", "", "ghs_TOKENC")
	// A source the control plane sends is not trusted: the worker's own resolution replaces it.
	forged := entry("job-d", "vm-1", "ghs_TOKEND")
	forged.Source = vm2

	sources := &fakeSources{addrs: map[string]netip.Addr{"vm-1": vm1}}
	var logs bytes.Buffer
	store := NewStore(0)
	store.now = func() time.Time { return now }
	s := &Syncer{
		Store:   store,
		Sources: sources,
		Fetch:   func(context.Context) ([]Entry, error) { return []Entry{a, b, noVM, forged}, nil },
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
		now:     func() time.Time { return now },
	}
	ctx := context.Background()
	lookup := func(e Entry, src netip.Addr) error {
		t.Helper()
		_, err := store.Lookup(e.Placeholder, "api.github.com", src)
		return err
	}

	s.Sync(ctx)
	if !s.Synced() {
		t.Fatal("a sync that left entries out is still a sync with the control plane")
	}
	if store.Len() != 2 {
		t.Fatalf("Len = %d, want only vm-1's two entries", store.Len())
	}
	if err := lookup(a, vm1); err != nil {
		t.Fatalf("vm-1's entry from vm-1: %v", err)
	}
	if err := lookup(a, vm2); !errors.Is(err, ErrWrongSource) {
		t.Fatalf("vm-1's entry from vm-2: %v, want ErrWrongSource", err)
	}
	if err := lookup(forged, vm2); !errors.Is(err, ErrWrongSource) {
		t.Fatalf("entry with a control-plane source, from that source: %v, want ErrWrongSource", err)
	}
	if err := lookup(forged, vm1); err != nil {
		t.Fatalf("entry with a control-plane source, from its VM: %v", err)
	}
	for name, e := range map[string]Entry{"unresolved VM": b, "no VM": noVM} {
		for _, src := range []netip.Addr{vm1, vm2, {}} {
			if err := lookup(e, src); !errors.Is(err, ErrUnknownPlaceholder) {
				t.Fatalf("%s, from %v: %v, want it not installed at all", name, src, err)
			}
		}
	}
	out := logs.String()
	for _, want := range []string{"vm=vm-2", "job-b", "job-c", errNoVM.Error()} {
		if !strings.Contains(out, want) {
			t.Fatalf("log does not say %q was left out:\n%s", want, out)
		}
	}
	for _, e := range []Entry{a, b, noVM, forged} {
		if strings.Contains(out, e.Token) || strings.Contains(out, e.Placeholder) {
			t.Fatalf("log carries a token or placeholder:\n%s", out)
		}
	}

	// Still unresolved on the next sync: left out again, reported once.
	logs.Reset()
	now = now.Add(time.Minute)
	s.Sync(ctx)
	if store.Len() != 2 || logs.Len() != 0 {
		t.Fatalf("Len = %d, log %q; want the same two entries and no repeated warning", store.Len(), logs.String())
	}

	// vm-2 gets its address: the next sync installs its entry, bound to it.
	sources.addrs["vm-2"] = vm2
	now = now.Add(time.Minute)
	s.Sync(ctx)
	if err := lookup(b, vm2); err != nil {
		t.Fatalf("vm-2's entry once resolved: %v", err)
	}
	if err := lookup(b, vm1); !errors.Is(err, ErrWrongSource) {
		t.Fatalf("vm-2's entry from vm-1: %v, want ErrWrongSource", err)
	}
	if err := lookup(noVM, vm1); !errors.Is(err, ErrUnknownPlaceholder) {
		t.Fatalf("entry with no VM: %v, want never installed", err)
	}

	// vm-1 restarts onto another address: the entry follows it, and the old address stops working.
	moved := netip.MustParseAddr("192.168.64.9")
	sources.addrs["vm-1"] = moved
	now = now.Add(time.Minute)
	s.Sync(ctx)
	if err := lookup(a, moved); err != nil {
		t.Fatalf("vm-1's entry from its new address: %v", err)
	}
	if err := lookup(a, vm1); !errors.Is(err, ErrWrongSource) {
		t.Fatalf("vm-1's entry from its old address: %v, want ErrWrongSource", err)
	}

	// vm-1 goes away: its entries leave the store even though the control plane still lists them.
	delete(sources.addrs, "vm-1")
	now = now.Add(time.Minute)
	s.Sync(ctx)
	if err := lookup(a, moved); !errors.Is(err, ErrUnknownPlaceholder) {
		t.Fatalf("entry of a VM that is gone: %v, want dropped", err)
	}
}

// A broker with no resolver installs nothing, and a resolver that answers for the empty name
// still binds nothing to it.
func TestSyncerWithoutASourceInstallsNothing(t *testing.T) {
	e := newEntry(t, "j", KindGitHub, "ghs_TOKEN")
	e.VM = "vm-1"
	e.Source = netip.Addr{}
	noVM := newEntry(t, "k", KindGitHub, "ghs_TOKEN2")
	noVM.Source = netip.Addr{}
	fetch := func(context.Context) ([]Entry, error) { return []Entry{e, noVM}, nil }

	store := NewStore(0)
	(&Syncer{Store: store, Fetch: fetch}).Sync(context.Background())
	if store.Len() != 0 {
		t.Fatalf("Len = %d, want nothing installed with no resolver", store.Len())
	}

	any := netip.MustParseAddr("192.168.64.5")
	store = NewStore(0)
	(&Syncer{Store: store, Fetch: fetch, Sources: &fakeSources{addrs: map[string]netip.Addr{"": any, "vm-1": any}}}).Sync(context.Background())
	if _, err := store.Lookup(noVM.Placeholder, "github.com", any); !errors.Is(err, ErrUnknownPlaceholder) {
		t.Fatalf("entry with no VM was installed: %v", err)
	}
	if _, err := store.Lookup(e.Placeholder, "github.com", any); err != nil {
		t.Fatalf("vm-1's entry: %v", err)
	}
}
