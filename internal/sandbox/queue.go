package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Queue puts the jobs waiting for a VM slot of a pool in order, first come, first served. Every
// `grove sandbox run` waiting for a slot holds a ticket: a file in Dir/<pool>, named by when the job
// joined, that it keeps locked. A job takes a free slot only once fewer jobs are ahead of it than
// there are free slots, so a job that joined later never overtakes one still waiting. The kernel
// drops a lock when its process exits, so a crashed runner's ticket stops counting at once and is
// cleaned up by the next runner that looks.
type Queue struct {
	Dir string
}

// Ticket is a job's place in its pool's queue.
type Ticket struct {
	path string
	file *os.File
}

// Join takes a ticket for job id (unique among running jobs) in pool's queue, placed by at.
func (q Queue) Join(pool, id string, at time.Time) (*Ticket, error) {
	dir := filepath.Join(q.Dir, pool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("queue directory: %w", err)
	}
	// The ticket is locked before it gets its real name, so no other runner ever sees it unlocked
	// and takes it for a crashed runner's.
	tmp, err := os.CreateTemp(dir, ".join-")
	if err != nil {
		return nil, fmt.Errorf("queue ticket: %w", err)
	}
	ok, err := tryLockFile(tmp)
	if err != nil || !ok {
		tmp.Close()
		os.Remove(tmp.Name())
		if err == nil {
			err = fmt.Errorf("a fresh ticket was already locked")
		}
		return nil, fmt.Errorf("lock queue ticket: %w", err)
	}
	// Zero-padded nanoseconds sort as strings in time order; the id breaks ties.
	path := filepath.Join(dir, fmt.Sprintf("%020d-%s.wait", at.UnixNano(), id))
	if err := os.Rename(tmp.Name(), path); err != nil {
		unlockFile(tmp)
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("queue ticket: %w", err)
	}
	return &Ticket{path: path, file: tmp}, nil
}

// Ahead counts the jobs still waiting that joined before this one. A ticket whose runner is gone is
// removed along the way.
func (t *Ticket) Ahead() (int, error) {
	if t == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(filepath.Dir(t.path))
	if err != nil {
		return 0, fmt.Errorf("read queue: %w", err)
	}
	mine := filepath.Base(t.path)
	ahead := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".wait") || name >= mine {
			continue
		}
		if waiting(filepath.Join(filepath.Dir(t.path), name)) {
			ahead++
		}
	}
	return ahead, nil
}

// waiting reports whether the ticket at path is still held, removing it when it isn't.
func waiting(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false // already gone
	}
	defer f.Close()
	ok, err := tryLockFile(f)
	if err != nil {
		return true // can't tell: count it, so order is never broken
	}
	if !ok {
		return true
	}
	// Nobody holds it: its runner exited without leaving.
	os.Remove(path)
	unlockFile(f)
	return false
}

// Leave gives up the ticket: the job got a slot, or stopped waiting.
func (t *Ticket) Leave() {
	if t == nil || t.file == nil {
		return
	}
	os.Remove(t.path)
	unlockFile(t.file)
	t.file.Close()
	t.file = nil
}
