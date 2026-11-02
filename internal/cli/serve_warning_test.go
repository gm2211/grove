package cli

import (
	"testing"
	"time"
)

func TestRepeatedWarning(t *testing.T) {
	var w repeatedWarning
	t0 := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)

	if !w.due("exec linux-studio-0: 503", t0) {
		t.Fatal("first failure should be logged")
	}
	if w.due("exec linux-studio-0: 503", t0.Add(time.Minute)) {
		t.Error("the same failure a minute later should not be logged again")
	}
	if !w.due("read node pool linux: 403", t0.Add(2*time.Minute)) {
		t.Error("a different failure should be logged at once")
	}
	if w.due("read node pool linux: 403", t0.Add(11*time.Minute)) {
		t.Error("the same failure within repeatWarningEvery of its last log should not be logged")
	}
	if !w.due("read node pool linux: 403", t0.Add(12*time.Minute)) {
		t.Error("a failure that persists should be logged again after repeatWarningEvery")
	}
	w.clear()
	if !w.due("read node pool linux: 403", t0.Add(13*time.Minute)) {
		t.Error("after a clean tick, the next failure should be logged at once")
	}
}

func TestRefusedSummaryIsSorted(t *testing.T) {
	got := refusedSummary(map[string]string{"linux-b-0": "its pool is sealed", "linux-a-0": "differs from its spec: shutdown script"})
	want := "linux-a-0: differs from its spec: shutdown script; linux-b-0: its pool is sealed"
	if got != want {
		t.Fatalf("refusedSummary = %q, want %q", got, want)
	}
	if refusedSummary(nil) != "" {
		t.Fatal("no refusals must summarise as empty")
	}
}
