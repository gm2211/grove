package orchard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeExecServer plays the controller's side of one exec WebSocket: it records every stdin
// frame, and once stdin is closed (an empty stdin frame) it answers with the scripted frames.
type fakeExecServer struct {
	mu     sync.Mutex
	stdin  bytes.Buffer
	closed bool
	reply  []execFrame
}

func (f *fakeExecServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var frame execFrame
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Errorf("unmarshal: %v", err)
				return
			}
			if frame.Type != execFrameStdin {
				continue
			}

			f.mu.Lock()
			if len(frame.Data) > 0 {
				f.stdin.Write(frame.Data)
				f.mu.Unlock()
				continue
			}
			f.closed = true
			f.mu.Unlock()
			break
		}

		for _, frame := range f.reply {
			payload, _ := json.Marshal(frame)
			if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
				return
			}
		}
	})
}

func dialExec(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestExecSession_StdinEOFClosesRemoteStdin(t *testing.T) {
	fake := &fakeExecServer{reply: []execFrame{
		{Type: execFrameStdout, Data: []byte("unpacked\n")},
		{Type: execFrameExit, Exit: &execExit{Code: 0}},
	}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	payload := bytes.Repeat([]byte("x"), 10000) // more than one 4 KiB stdin frame
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := newExecSession(ctx, dialExec(t, srv), bytes.NewReader(payload), nil)
	defer session.Close()

	out, err := io.ReadAll(session.Output())
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	code, err := session.Wait(ctx)
	if err != nil || code != 0 {
		t.Fatalf("Wait = %d, %v; want 0, nil", code, err)
	}
	if string(out) != "unpacked\n" {
		t.Fatalf("output = %q", out)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.closed {
		t.Fatal("the remote command's stdin was never closed, so `tar -x` would wait forever")
	}
	if !bytes.Equal(fake.stdin.Bytes(), payload) {
		t.Fatalf("remote stdin got %d bytes, want %d", fake.stdin.Len(), len(payload))
	}
}

func TestExecSession_SeparateStderr(t *testing.T) {
	fake := &fakeExecServer{reply: []execFrame{
		{Type: execFrameStdout, Data: []byte("out\n")},
		{Type: execFrameStderr, Data: []byte("err\n")},
		{Type: execFrameExit, Exit: &execExit{Code: 3}},
	}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	session := newExecSession(ctx, dialExec(t, srv), strings.NewReader(""), &stderr)
	defer session.Close()

	out, err := io.ReadAll(session.Output())
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	code, err := session.Wait(ctx)
	if err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v; want 3, nil", code, err)
	}
	if string(out) != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("stdout = %q, stderr = %q", out, stderr.String())
	}
}

func TestExecSession_CombinedOutputByDefault(t *testing.T) {
	fake := &fakeExecServer{reply: []execFrame{
		{Type: execFrameStdout, Data: []byte("out\n")},
		{Type: execFrameStderr, Data: []byte("err\n")},
		{Type: execFrameExit, Exit: &execExit{Code: 0}},
	}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := newExecSession(ctx, dialExec(t, srv), strings.NewReader(""), nil)
	defer session.Close()

	out, err := io.ReadAll(session.Output())
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(out) != "out\nerr\n" {
		t.Fatalf("output = %q", out)
	}
}

func TestExecSession_StreamClosedWithoutExitIsNotSuccess(t *testing.T) {
	for name, reply := range map[string][]execFrame{
		"no exit frame":     {{Type: execFrameStdout, Data: []byte("partial\n")}},
		"exit with no code": {{Type: execFrameExit}},
	} {
		fake := &fakeExecServer{reply: reply}
		srv := httptest.NewServer(fake.handler(t))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		session := newExecSession(ctx, dialExec(t, srv), strings.NewReader(""), nil)

		_, readErr := io.ReadAll(session.Output())
		code, err := session.Wait(ctx)
		if !errors.Is(err, ErrExecNoExitStatus) || !errors.Is(readErr, ErrExecNoExitStatus) {
			t.Errorf("%s: Wait = %d, %v (output: %v); want ErrExecNoExitStatus, never a bare 0", name, code, err, readErr)
		}

		_ = session.Close()
		cancel()
		srv.Close()
	}
}
