package nomad

import (
	"context"
	"errors"
	"io"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
)

// TestStreamLogs_FramesDrainedBeforeConcurrentErrSignal reproduces the actual root cause of the
// "/logs may silently drop lines" defect: AllocFS().Logs() can signal completion (closing frames,
// or sending a value on errCh — even a benign one) at effectively the same instant it finishes
// delivering the last frame(s) of an already-terminal allocation's logs. A plain
// `select { case <-frames: ...; case <-errCh: ... }` picks uniformly at random among ready cases,
// so without draining frames first, this exact setup (frames fully buffered and closed, errCh
// already holding a value) had roughly a 50% chance of losing every already-queued frame.
//
// This was reproduced live against a real Nomad server: the identical request for one finished
// job's logs (2 stdout + 2 stderr lines) returned all 4, some, or 0 lines across otherwise-identical
// repeats, while hitting Nomad's own /v1/client/fs/logs endpoint directly for the same allocation
// was 100% consistent every time — proving the data was never actually lost in Nomad, only dropped
// by this select race on the way out through grove.
func TestStreamLogs_FramesDrainedBeforeConcurrentErrSignal(t *testing.T) {
	want := "line1\nline2\nline3\nline4\n"

	// Run many times: before the fix this test would only intermittently fail (Go's select choice
	// is random), so a single run isn't a reliable regression guard. After the fix it must pass
	// every time — frames are drained deterministically before errCh is ever considered.
	for i := 0; i < 200; i++ {
		frames := make(chan *nomadapi.StreamFrame, 4)
		errCh := make(chan error, 1)

		frames <- &nomadapi.StreamFrame{Data: []byte("line1\n")}
		frames <- &nomadapi.StreamFrame{Data: []byte("line2\n")}
		frames <- &nomadapi.StreamFrame{Data: []byte("line3\n")}
		frames <- &nomadapi.StreamFrame{Data: []byte("line4\n")}
		close(frames)
		// Simulate AllocFS().Logs() signalling "done" on errCh at the same instant frames closes.
		errCh <- io.EOF

		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			streamLogs(context.Background(), frames, errCh, pw)
			close(done)
		}()

		got, err := io.ReadAll(pr)
		<-done
		if err != nil {
			t.Fatalf("iteration %d: ReadAll: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("iteration %d: got %q, want %q", i, got, want)
		}
	}
}

// TestStreamLogs_ClosesWithErrorOnceFramesDrained covers the complementary case: a genuine,
// non-benign error must still be surfaced (not silently swallowed) once there is nothing left
// buffered on frames to drain first.
func TestStreamLogs_ClosesWithErrorOnceFramesDrained(t *testing.T) {
	frames := make(chan *nomadapi.StreamFrame, 1)
	errCh := make(chan error, 1)

	frames <- &nomadapi.StreamFrame{Data: []byte("only line\n")}
	wantErr := errors.New("boom")
	errCh <- wantErr
	// frames stays open (not closed) here: a real transport failure mid-stream, not a clean EOF.

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		streamLogs(context.Background(), frames, errCh, pw)
		close(done)
	}()

	got, err := io.ReadAll(pr)
	<-done
	if string(got) != "only line\n" {
		t.Fatalf("got %q, want %q", got, "only line\n")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestStreamLogs_CtxDoneStopsStream covers the third exit path: ctx cancellation ends the stream
// with ctx.Err(), matching watchForTerminal's use of cancel() to sever a follow stream once its job
// goes terminal (see internal/dispatch/service.go).
func TestStreamLogs_CtxDoneStopsStream(t *testing.T) {
	frames := make(chan *nomadapi.StreamFrame)
	errCh := make(chan error)

	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		streamLogs(ctx, frames, errCh, pw)
		close(done)
	}()

	cancel()

	_, err := io.ReadAll(pr)
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
