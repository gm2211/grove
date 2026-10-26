package broker

import (
	"errors"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	r := NewRegistry(2)
	r.now = func() time.Time { return now }

	a1, a2 := newEntry(t, "job-a", KindGitHub, "ta1"), newEntry(t, "job-a", KindAnthropic, "ta2")
	b1 := newEntry(t, "job-b", KindGitHub, "tb1")
	for _, add := range []struct {
		worker string
		e      Entry
	}{{"mac-1", a1}, {"mac-1", a2}, {"mac-2", b1}} {
		if err := r.Add(add.worker, add.e); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Add("mac-1", newEntry(t, "job-c", KindGitHub, "tc")); !errors.Is(err, ErrTooMany) {
		t.Fatalf("over the per-worker cap: %v", err)
	}
	if err := r.Add("", newEntry(t, "job-c", KindGitHub, "tc")); err == nil {
		t.Fatal("entry with no worker accepted")
	}
	if err := r.Add("mac-1", Entry{Placeholder: "nope", Kind: KindGitHub, Token: "t"}); err == nil {
		t.Fatal("malformed entry accepted")
	}

	got := r.For("mac-1")
	if len(got) != 2 || got[0].Placeholder > got[1].Placeholder {
		t.Fatalf("For(mac-1) = %+v", got)
	}
	if !got[0].Expires.Equal(now.Add(Lease)) {
		t.Fatalf("lease = %v, want %v", got[0].Expires, now.Add(Lease))
	}
	if len(r.For("mac-3")) != 0 {
		t.Fatal("unknown worker got entries")
	}

	// job-a keeps renewing; job-b stops and its lease runs out.
	now = now.Add(Lease - time.Minute)
	if n := r.Renew("job-a"); n != 2 {
		t.Fatalf("Renew = %d", n)
	}
	now = now.Add(2 * time.Minute)
	if len(r.For("mac-1")) != 2 || len(r.For("mac-2")) != 0 || r.Len() != 2 {
		t.Fatalf("after job-b's lease ran out: mac-1 %d, mac-2 %d, total %d", len(r.For("mac-1")), len(r.For("mac-2")), r.Len())
	}
	if n := r.RemoveJob("job-a"); n != 2 || r.Len() != 0 {
		t.Fatalf("RemoveJob = %d, Len = %d", n, r.Len())
	}
}
