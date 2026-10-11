package sandbox

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// logStream turns one stream of `docker logs --timestamps` output back into the job's own output.
// Every line docker prints starts with the time the job wrote it, which makes the stream
// resumable: when Runner reattaches after a dropped connection (with --since), lines at or before
// the last one already written are repeats and get dropped.
type logStream struct {
	mu  sync.Mutex
	out io.Writer
	// last is the timestamp of the last line written to out.
	last time.Time
	// partial holds a line whose newline hasn't arrived yet in the current attach.
	partial []byte
	// resume is where a reattach must start so a partial line dropped by reset is fetched whole.
	resume time.Time
}

func newLogStream(out io.Writer) *logStream { return &logStream{out: out} }

func (l *logStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		l.emit(l.partial[:i+1])
		l.partial = l.partial[i+1:]
	}
	return len(p), nil
}

// emit writes one line, minus its timestamp, unless it was already written.
func (l *logStream) emit(line []byte) {
	ts, rest, ok := splitTimestamp(line)
	if ok {
		if !ts.After(l.last) {
			return
		}
		l.last = ts
	}
	_, _ = l.out.Write(rest)
}

// reset ends an attach that may have been cut off: a partial line is dropped (the next attach
// fetches it whole) rather than written half.
func (l *logStream) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		if ts, _, ok := splitTimestamp(l.partial); ok && (l.resume.IsZero() || ts.Before(l.resume)) {
			l.resume = ts
		}
	}
	l.partial = nil
}

// flush writes a trailing partial line: after the final fetch, it is the job's last output, which
// simply had no newline.
func (l *logStream) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.emit(l.partial)
		l.partial = nil
	}
}

// resumePoint is the earliest time a reattach must cover for this stream: zero when nothing has
// arrived yet (fetch from the start).
func (l *logStream) resumePoint() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.resume.IsZero() {
		return l.resume
	}
	return l.last
}

// since is the --since value covering every stream, or "" to fetch everything. docker includes
// lines stamped exactly at --since, which emit then drops as repeats.
func since(streams ...*logStream) string {
	var at time.Time
	for _, s := range streams {
		p := s.resumePoint()
		if p.IsZero() {
			return ""
		}
		if at.IsZero() || p.Before(at) {
			at = p
		}
	}
	if at.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond())
}

// splitTimestamp splits docker's "<RFC3339Nano> " line prefix off line.
func splitTimestamp(line []byte) (time.Time, []byte, bool) {
	i := bytes.IndexByte(line, ' ')
	if i <= 0 {
		return time.Time{}, line, false
	}
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(line[:i])))
	if err != nil {
		return time.Time{}, line, false
	}
	return ts, line[i+1:], true
}
