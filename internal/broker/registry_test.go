package broker

import (
	"errors"
	"testing"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry(3)
	a1, a2 := newEntry(t, "job-a", KindGitHub, "ta1"), newEntry(t, "job-a", KindAnthropic, "ta2")
	b1 := newEntry(t, "job-b", KindGitHub, "tb1")
	if err := r.Put("job-a", []Entry{a1, a2}); err != nil {
		t.Fatal(err)
	}
	if err := r.Put("job-b", []Entry{b1}); err != nil {
		t.Fatal(err)
	}
	if err := r.Put("job-c", []Entry{newEntry(t, "job-c", KindGitHub, "tc")}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("over the cap: %v", err)
	}
	if err := r.Put("job-b", []Entry{a1}); err == nil {
		t.Fatal("job-a's entry accepted under job-b")
	}
	if err := r.Put("job-b", []Entry{{Placeholder: "nope", Job: "job-b", Kind: KindGitHub, Token: "t"}}); err == nil {
		t.Fatal("malformed entry accepted")
	}
	if err := r.Put("", nil); err == nil {
		t.Fatal("entries with no job accepted")
	}

	// Replacing a job's entries (a token refresh) doesn't count the old ones against the cap.
	a1.Token = "ta1-refreshed"
	if err := r.Put("job-a", []Entry{a1, a2}); err != nil {
		t.Fatal(err)
	}
	if got := r.Job("job-a"); len(got) != 2 || got[0].Token != "ta1-refreshed" {
		t.Fatalf("Job(job-a) = %+v", got)
	}
	if got := r.Jobs(); len(got) != 2 || got[0] != "job-a" || got[1] != "job-b" {
		t.Fatalf("Jobs = %v", got)
	}
	if n := r.RemoveJob("job-a"); n != 2 || r.Len() != 1 {
		t.Fatalf("RemoveJob = %d, Len = %d", n, r.Len())
	}
	if err := r.Put("job-b", nil); err != nil || r.Len() != 0 || len(r.Jobs()) != 0 {
		t.Fatalf("Put(nil) = %v, Len %d", err, r.Len())
	}
}
