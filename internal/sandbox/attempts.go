package sandbox

import (
	"fmt"
	"os"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/orchard"
)

// attemptLog keeps the job's dispatch.AttemptRecord and writes it to dir after every change, so
// grove serve sees where the job is running while it runs. Without a dir or a job id it only counts.
type attemptLog struct {
	dir    string
	record dispatch.AttemptRecord
}

// start records an attempt beginning in vm and returns its index.
func (l *attemptLog) start(vm orchard.VM) int {
	l.record.Attempts = append(l.record.Attempts, dispatch.Attempt{
		VM: vm.Name, VMUID: vm.UID, Worker: machine(vm),
		StartedAt: time.Now().UTC(), Outcome: dispatch.AttemptRunning,
	})
	l.save()
	return len(l.record.Attempts) - 1
}

// drop forgets attempt i: the VM refused the job because it was shutting down, so nothing ran.
func (l *attemptLog) drop(i int) {
	l.record.Attempts = append(l.record.Attempts[:i], l.record.Attempts[i+1:]...)
	l.save()
}

// finish records how attempt i ended: the code job.run returned, whether the job's script had
// started, the runner's last message (which says why a VM was lost), and whether the job was
// being canceled.
func (l *attemptLog) finish(i int, code int, started bool, lastMessage string, canceled bool) {
	a := &l.record.Attempts[i]
	now := time.Now().UTC()
	a.FinishedAt = &now
	a.JobStarted = started
	switch {
	case canceled || code == ExitCanceled:
		a.Outcome = dispatch.AttemptCanceled
	case code == ExitLost:
		a.Outcome = dispatch.AttemptLost
		a.Reason = lastMessage
	case !started:
		a.Outcome = dispatch.AttemptSetup
		a.Reason = lastMessage
	default:
		a.Outcome = dispatch.AttemptExited
		c := code
		a.ExitCode = &c
	}
	l.save()
}

func (l *attemptLog) quarantine(reason string) {
	l.record.Quarantined = true
	l.record.QuarantineReason = reason
	l.save()
}

func (l *attemptLog) count() int { return len(l.record.Attempts) }

func (l *attemptLog) save() {
	if l.dir == "" || l.record.JobID == "" {
		return
	}
	if err := dispatch.WriteAttempts(l.dir, l.record); err != nil {
		// The job runs on regardless; grove serve just can't show where.
		fmt.Fprintf(os.Stderr, "grove: could not record the job's attempt: %v\n", err)
	}
}
