package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A job's output is kept in one file on the control plane, <LogDir>/<id>.log, one JSON record per
// line of output: {"t": RFC3339Nano time received, "s": "stdout"|"stderr", "l": the line}. One
// file keeps the order lines arrived in across both streams; the stream tag keeps them apart.
// Logs (raw bytes) and LogLines (tagged lines) both read it, following it while the job runs. The
// file is written only by the job's own executor goroutine and deleted by RunReconciler under the
// same retention as artifacts.

// maxLogLine is the longest line kept in one record; a longer one is split.
const maxLogLine = 64 * 1024

type logRecord struct {
	T time.Time `json:"t"`
	S string    `json:"s"`
	L string    `json:"l"`
}

// logFile is one job's log, open for appending while the job runs.
type logFile struct {
	mu     sync.Mutex
	f      *os.File
	now    func() time.Time
	stdout *logStream
	stderr *logStream
}

type logStream struct {
	lf      *logFile
	name    string
	partial []byte
}

func openLogFile(path string) (*logFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	lf := &logFile{f: f, now: func() time.Time { return time.Now().UTC() }}
	lf.stdout = &logStream{lf: lf, name: "stdout"}
	lf.stderr = &logStream{lf: lf, name: "stderr"}
	return lf, nil
}

// Write implements io.Writer for one stream: complete lines become records, a trailing partial
// line waits for its newline (or Close).
func (s *logStream) Write(p []byte) (int, error) {
	s.lf.mu.Lock()
	defer s.lf.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.partial = append(s.partial, p...)
			for len(s.partial) >= maxLogLine {
				if err := s.lf.writeLocked(s.name, s.partial[:maxLogLine]); err != nil {
					return n, err
				}
				s.partial = append([]byte(nil), s.partial[maxLogLine:]...)
			}
			break
		}
		line := append(s.partial, p[:i]...)
		s.partial = nil
		line = bytes.TrimSuffix(line, []byte("\r"))
		for len(line) > maxLogLine {
			if err := s.lf.writeLocked(s.name, line[:maxLogLine]); err != nil {
				return n, err
			}
			line = line[maxLogLine:]
		}
		if err := s.lf.writeLocked(s.name, line); err != nil {
			return n, err
		}
		p = p[i+1:]
	}
	return n, nil
}

// writeLocked appends one record in a single write, so a concurrent reader never sees half of a
// record except at the very end of the file (which it waits out).
func (lf *logFile) writeLocked(stream string, line []byte) error {
	data, err := json.Marshal(logRecord{T: lf.now(), S: stream, L: string(line)})
	if err != nil {
		return err
	}
	_, err = lf.f.Write(append(data, '\n'))
	return err
}

// Close writes out any unterminated final lines and closes the file.
func (lf *logFile) Close() error {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	for _, s := range []*logStream{lf.stdout, lf.stderr} {
		if len(s.partial) > 0 {
			_ = lf.writeLocked(s.name, s.partial)
			s.partial = nil
		}
	}
	return lf.f.Close()
}

// logPath is where job id's log lives.
func (s *service) logPath(id string) string {
	return filepath.Join(s.logDir, id+".log")
}

// openLogStream waits (with follow) for job id's log to exist and returns it open for reading.
// A nil file with a nil error means the job is over and has no log (it never started, or its log
// expired): the caller streams nothing.
func (s *service) openLogStream(ctx context.Context, id string, follow bool) (*os.File, error) {
	for {
		rec, ok := s.store.get(id)
		if !ok {
			return nil, ErrNotFound
		}
		f, err := os.Open(s.logPath(id))
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if isTerminal(rec.Status) {
			return nil, nil
		}
		if !follow {
			return nil, ErrNoAllocationYet
		}
		select {
		case <-ctx.Done():
			return nil, ErrNoAllocationYet
		case <-time.After(s.opts.LogPollInterval):
		}
	}
}

// tailLog sends every record of f to out, following it until the job is terminal and the file
// drained when follow is set, then closes out and f.
func (s *service) tailLog(ctx context.Context, id string, f *os.File, follow bool, out chan<- LogLine) {
	defer close(out)
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	var pending []byte
	finalPass := false
	for {
		chunk, err := r.ReadBytes('\n')
		pending = append(pending, chunk...)
		if n := len(pending); n > 0 && pending[n-1] == '\n' {
			var rec logRecord
			if jerr := json.Unmarshal(pending, &rec); jerr == nil {
				select {
				case out <- LogLine{Stream: rec.S, Line: rec.L, Time: rec.T}:
				case <-ctx.Done():
					return
				}
			}
			pending = nil
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}
		// End of the file so far (possibly half a record in, its rest still being written).
		if !follow || finalPass {
			return
		}
		if job, ok := s.store.get(id); !ok || isTerminal(job.Status) {
			// The executor closes the log before recording the end: one more pass reads all of it.
			finalPass = true
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.opts.LogPollInterval):
		}
	}
}

// LogLines implements Service.LogLines.
func (s *service) LogLines(ctx context.Context, id string, follow bool) (<-chan LogLine, error) {
	f, err := s.openLogStream(ctx, id, follow)
	if err != nil {
		return nil, err
	}
	out := make(chan LogLine, 64)
	if f == nil {
		close(out)
		return out, nil
	}
	go s.tailLog(ctx, id, f, follow, out)
	return out, nil
}

// Logs implements Service.Logs: the same lines as LogLines, each followed by a newline.
func (s *service) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	lines, err := s.LogLines(ctx, id, follow)
	if err != nil {
		cancel()
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		defer cancel()
		for ln := range lines {
			if _, err := io.WriteString(pw, ln.Line+"\n"); err != nil {
				cancel()
				for range lines {
				}
				return
			}
		}
		pw.Close()
	}()
	return &cancelOnCloseReader{ReadCloser: pr, cancel: cancel}, nil
}

// cancelOnCloseReader wraps a ReadCloser so that Close also stops the goroutine feeding it.
type cancelOnCloseReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseReader) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}
