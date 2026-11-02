package conformance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/snooze"
)

// L4 (spec/README.md): a job waiting only because every worker of its pool is snoozed is never
// failed for it, including in the moments after the snooze ends and before the Expirer and the
// fleet tick have caught up (the worker is still paused in Orchard, and PoolHealth still says so).
func TestL4_JobQueuedBehindSnoozeSurvivesSnoozeEnd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		until    time.Duration // snooze end, relative to now
		expire   bool          // the Expirer resumed the worker and cleared the snooze after the fleet tick listed it
		operator bool          // no snooze at all: an operator paused the pool's only worker
		want     dispatch.Status
	}{
		{name: "while snoozed", until: time.Hour, want: dispatch.StatusPending},
		{name: "just after the snooze ended", until: -time.Second, want: dispatch.StatusPending},
		{name: "after the Expirer resumed it, before the next fleet tick", until: -time.Second, expire: true, want: dispatch.StatusPending},
		// Not vacuous: a pool paused by an operator still fails its job fast.
		{name: "operator pause", operator: true, want: dispatch.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pauses, err := snooze.NewStore(filepath.Join(t.TempDir(), "pauses.json"))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if tc.operator {
				if err := pauses.SetOperator("mac1", "ops", now); err != nil {
					t.Fatal(err)
				}
			} else if err := pauses.SetSnooze("mac1", snooze.Snooze{Until: now.Add(tc.until), Mode: snooze.ModeDrain, At: now.Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			health := &fleet.PoolHealth{Pauses: pauses}
			// The last fleet tick listed mac1, the pool's only worker, paused in Orchard.
			paused := []fleet.PlannedPause{{Pool: "macos", Workers: []string{"mac1"}}}
			health.ObserveFleet(nil, fleet.Plan{Paused: paused, PausedOnly: paused, ListedAt: now.Add(-time.Second)})
			if tc.expire {
				if err := pauses.ClearSnooze("mac1"); err != nil {
					t.Fatal(err)
				}
			}

			nc := &fakeNomad{evals: noNodesInPool("macos")}
			svc, err := dispatch.New(nc, dispatch.Options{
				StorePath:        filepath.Join(t.TempDir(), "jobs.json"),
				AttemptsDir:      t.TempDir(),
				StatusTTL:        time.Millisecond,
				PoolHealth:       health,
				UnplaceableGrace: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := svc.Submit(context.Background(), dispatch.JobRequest{Kind: dispatch.KindShell, Pool: "macos", Script: "true"})
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)

			got, err := svc.Get(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.want {
				reason := ""
				if got.FailureReason != nil {
					reason = *got.FailureReason
				}
				t.Fatalf("status = %s (%s), want %s", got.Status, reason, tc.want)
			}
		})
	}
}
