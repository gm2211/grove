package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/install"
)

func TestKeychainHandoff_RetriesUntilAReaderThenForgets(t *testing.T) {
	now := time.Unix(0, 0)
	var got []string
	reader := false
	k := newKeychainHandoff()
	k.path = func() (string, error) { return "/fifo", nil }
	k.now = func() time.Time { return now }
	k.deliver = func(_, pw string) error {
		if !reader {
			return install.ErrNoUnlockReader
		}
		got = append(got, pw)
		return nil
	}
	if k.try() {
		t.Fatal("pending with nothing offered")
	}
	k.offer("pw")
	if !k.try() || len(got) != 0 {
		t.Fatal("with no reader it should stay pending")
	}
	reader = true
	if k.try() || len(got) != 1 || got[0] != "pw" {
		t.Fatalf("delivered %v", got)
	}
	if k.try() || len(got) != 1 {
		t.Fatal("delivered twice")
	}

	reader = false
	k.offer("late")
	now = now.Add(keychainHandoffTTL + time.Second)
	if k.try() {
		t.Fatal("kept a password past its TTL")
	}

	k.deliver = func(string, string) error { return errors.New("not a FIFO") }
	k.offer("x")
	if k.try() || k.password != "" {
		t.Fatal("kept a password the FIFO refused")
	}
}
