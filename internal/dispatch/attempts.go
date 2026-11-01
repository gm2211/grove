package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MaxAttempts is how many times a sealed pool's job is run when its VM keeps dying under it: the
// first run and up to two reruns. Only a lost VM starts another attempt; a job whose own script
// fails, times out or is canceled is never rerun.
const MaxAttempts = 3

// QuarantineMachines is on how many different machines (worker Macs) a job may be lost before
// grove decides the job itself is what kills them, a poison pill, and stops running it.
const QuarantineMachines = 2

// Attempt outcomes.
const (
	AttemptRunning  = "running"
	AttemptExited   = "exited"   // the job ran to its end; ExitCode is its exit code
	AttemptLost     = "lost"     // the VM stopped answering while the job was in it
	AttemptSetup    = "setup"    // grove could not set the job up in the VM
	AttemptCanceled = "canceled" // the job was canceled while in the VM
	// AttemptEvicted: the VM's Mac was snoozed with --now and grove stopped the job to run it in
	// another VM. Not a loss: it never counts towards quarantine, MaxAttempts or machine health.
	AttemptEvicted = "evicted"
)

// Attempt is one run of a job in one VM.
type Attempt struct {
	VM    string `json:"vm"`
	VMUID string `json:"vmUid,omitempty"`
	// Worker is the Mac the VM runs on.
	Worker     string     `json:"worker,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Outcome    string     `json:"outcome"`
	// JobStarted is set once the job's script was started in the VM. A VM lost before that says
	// nothing about the job, so only a loss after it counts towards quarantining the job.
	JobStarted bool `json:"jobStarted,omitempty"`
	ExitCode   *int `json:"exitCode,omitempty"`
	// Reason says why the attempt ended the way it did, for a lost or setup attempt.
	Reason string `json:"reason,omitempty"`
}

// AttemptRecord is everything the job's driver (`grove sandbox run`) tells grove about where and
// how a sealed pool's job ran. The driver writes it on the control plane, where the job never
// runs, so nothing in it comes from the job itself.
type AttemptRecord struct {
	JobID    string    `json:"jobId"`
	Attempts []Attempt `json:"attempts"`
	// Quarantined is set when the job was lost on QuarantineMachines different machines: grove
	// stopped running it so it can't take down another one.
	Quarantined      bool   `json:"quarantined,omitempty"`
	QuarantineReason string `json:"quarantineReason,omitempty"`
}

// jobIDPattern is what randomID mints; anything else is not a job id and never names a file.
var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// DefaultAttemptsDir is where drivers write attempt records: next to the job history.
func DefaultAttemptsDir() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "grove", "attempts")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "grove", "attempts")
}

// JobIDFromArtifactPrefix recovers the grove job id from the artifact_prefix every dispatch
// carries ("jobs/<id>/"), which is how a driver learns which job it runs.
func JobIDFromArtifactPrefix(prefix string) (string, bool) {
	id, ok := strings.CutPrefix(prefix, "jobs/")
	id, ok2 := strings.CutSuffix(id, "/")
	if !ok || !ok2 || !jobIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

func attemptsPath(dir, id string) (string, error) {
	if dir == "" {
		return "", errors.New("dispatch: no attempts directory")
	}
	if !jobIDPattern.MatchString(id) {
		return "", fmt.Errorf("dispatch: %q is not a job id", id)
	}
	return filepath.Join(dir, id+".json"), nil
}

// WriteAttempts stores rec in dir through a same-directory rename, so a reader never sees half
// of it.
func WriteAttempts(dir string, rec AttemptRecord) error {
	path, err := attemptsPath(dir, rec.JobID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+rec.JobID+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadAttempts loads job id's record from dir; ok is false when the driver wrote none.
func ReadAttempts(dir, id string) (rec AttemptRecord, ok bool, err error) {
	path, err := attemptsPath(dir, id)
	if err != nil {
		return AttemptRecord{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return AttemptRecord{}, false, nil
	}
	if err != nil {
		return AttemptRecord{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return AttemptRecord{}, false, fmt.Errorf("dispatch: parse %s: %w", path, err)
	}
	return rec, true, nil
}

// RemoveAttempts deletes job id's record, if any.
func RemoveAttempts(dir, id string) error {
	path, err := attemptsPath(dir, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
