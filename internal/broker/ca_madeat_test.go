package broker

import (
	"testing"
	"time"
)

// MadeAt is the moment the CA was created, kept across restarts: the control plane rebuilds the
// VMs that booted before it (fleet.BrokerCAs), and must not rebuild them again on every restart.
func TestCAMadeAtIsItsCreationAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Truncate(time.Second)
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Second)
	if made := ca.MadeAt(); made.Before(before) || made.After(after) {
		t.Fatalf("MadeAt = %v, want between %v and %v", made, before, after)
	}
	again, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.MadeAt().Equal(ca.MadeAt()) {
		t.Fatalf("after a restart MadeAt = %v, want %v", again.MadeAt(), ca.MadeAt())
	}
}
