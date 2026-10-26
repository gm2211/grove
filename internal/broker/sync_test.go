package broker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSyncer(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	e := newEntry(t, "j", KindGitHub, "t")
	e.Expires = now.Add(Lease)
	var fetches int
	var next []Entry
	var fail error
	store := NewStore(0)
	store.now = func() time.Time { return now }
	s := &Syncer{
		Store: store,
		Fetch: func(context.Context) ([]Entry, error) {
			fetches++
			return next, fail
		},
		now: func() time.Time { return now },
	}
	ctx := context.Background()

	next = []Entry{e}
	s.Sync(ctx)
	if store.Len() != 1 || fetches != 1 {
		t.Fatalf("Len %d, fetches %d", store.Len(), fetches)
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
