package fleet

import (
	"os"
	"testing"
)

// TestMain keeps tests that run a whole startup script from waiting two minutes for a credential
// broker that isn't there (see brokerFetchTries).
func TestMain(m *testing.M) {
	brokerFetchTries = 1
	os.Exit(m.Run())
}
