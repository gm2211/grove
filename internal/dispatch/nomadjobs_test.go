package dispatch

import (
	"context"
	"strings"
	"testing"
)

func TestEnsureJobs_RegistersAllKindsPerPool(t *testing.T) {
	nc := &fakeNomad{}
	pools := []string{"linux", "macos"}
	if err := EnsureJobs(context.Background(), nc, pools); err != nil {
		t.Fatalf("EnsureJobs: %v", err)
	}

	want := len(allKinds) * len(pools)
	if len(nc.registerCalls) != want {
		t.Fatalf("registered %d jobs, want %d", len(nc.registerCalls), want)
	}

	for _, kind := range allKinds {
		for _, pool := range pools {
			name := `job "grove-` + string(kind) + "-" + pool + `"`
			found := false
			for _, hcl := range nc.registerCalls {
				if strings.Contains(hcl, name) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("no registered job HCL found containing %s", name)
			}
		}
	}
}

func TestEnsureJobs_PropagatesRegisterError(t *testing.T) {
	nc := &fakeNomad{registerErr: context.DeadlineExceeded}
	if err := EnsureJobs(context.Background(), nc, []string{"linux"}); err == nil {
		t.Fatal("expected an error")
	}
}
