package conformance

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/health"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/server"
	"github.com/gm2211/grove/internal/snooze"
)

// TestSnoozeConformsToModel replays traces of spec/quint/snooze.qnt (module snoozeMBT, sampled
// by `quint run --out-itf` in spec/check.sh) against the real code: snooze.Engage, snooze.Wake,
// POST /workers/{name}/pause and /resume on the real HTTP server, health.Monitor.Tick and
// snooze.Expirer.Tick, on one worker with a fake Orchard. After every request it compares
// Orchard's pause bit and pauses.json's reasons with the model's state. A difference fails the
// build: the Go code no longer does what TLC proved S1, L1 and L2 for.
//
// Requests are replayed one at a time. That they really are atomic is TestS1_WritersHoldTheWorkersLock
// (every writer holds the worker's lock), so the chain is: lock ⇒ requests serialize; this test ⇒
// each request does what the model's step does; TLC ⇒ the model has the properties.
//
// GROVE_SNOOZE_TRACES names the directory of *.itf.json traces; without it the test skips.
func TestSnoozeConformsToModel(t *testing.T) {
	dir := os.Getenv("GROVE_SNOOZE_TRACES")
	if dir == "" {
		t.Skip("GROVE_SNOOZE_TRACES not set (spec/check.sh sets it)")
	}
	traces, err := tracesIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) == 0 {
		t.Fatalf("no traces in %s", dir)
	}
	requests := 0
	for _, tr := range traces {
		requests += replaySnoozeTrace(t, tr)
	}
	t.Logf("%d traces, %d requests replayed", len(traces), requests)
	if requests < len(traces) {
		t.Fatalf("only %d requests across %d traces: the traces exercise nothing", requests, len(traces))
	}
}

// startLabel and endLabel delimit one request (one Go call) per model process.
var (
	startLabel = map[string]string{"E": "e0", "F": "e0", "W": "w0", "R": "r0", "P": "p0", "H": "h0", "X": "x0"}
	endLabels  = map[string][]string{"E": {"done"}, "F": {"done"}, "W": {"done"}, "R": {"done", "refused"}, "P": {"done"},
		"H": {"done"}, "X": {"x0"}}
)

// pauseClient is the Orchard client the server and the health monitor see: fakePauser for the pause
// bit, and no VMs (the monitor's rebuild check finds none to delete).
type pauseClient struct {
	orchard.Client
	p *fakePauser
}

func (c pauseClient) ListWorkers(ctx context.Context) ([]orchard.Worker, error) {
	return c.p.ListWorkers(ctx)
}
func (c pauseClient) PauseWorker(ctx context.Context, n string) error { return c.p.PauseWorker(ctx, n) }
func (c pauseClient) ResumeWorker(ctx context.Context, n string) error {
	return c.p.ResumeWorker(ctx, n)
}
func (c pauseClient) GetVM(context.Context, string) (*orchard.VM, error) {
	return nil, nil
}

type lossJobs struct {
	dispatch.Service
	jobs []dispatch.Job
}

func (l lossJobs) List(context.Context) ([]dispatch.Job, error) { return l.jobs, nil }

type snoozeWorld struct {
	t       *testing.T
	p       *fakePauser
	st      *snooze.Store
	srv     *server.Server
	mon     *health.Monitor
	expirer *snooze.Expirer
}

func newSnoozeWorld(t *testing.T, operatorPaused bool) *snoozeWorld {
	t.Helper()
	st, err := snooze.NewStore(filepath.Join(t.TempDir(), "pauses.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePauser{paused: operatorPaused}
	if operatorPaused {
		if err := st.SetOperator("mac1", "giulio", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	oc := pauseClient{p: p}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Three jobs lost on three VMs of mac1 in the last hour: over the monitor's machine threshold,
	// under its per-VM one, so health.Monitor.Tick pauses mac1 exactly as the model's H does.
	finished := time.Now().Add(-time.Minute)
	var jobs []dispatch.Job
	for i, vm := range []string{"vm-a", "vm-b", "vm-c"} {
		jobs = append(jobs, dispatch.Job{ID: string(rune('a' + i)), Attempts: []dispatch.Attempt{{
			VM: vm, VMUID: vm, Worker: "mac1", Outcome: dispatch.AttemptLost, FinishedAt: &finished,
		}}})
	}
	return &snoozeWorld{
		t: t, p: p, st: st,
		srv:     server.New(oc, nil, nil, nil, server.Options{Token: "operator", Pauses: st, Logger: quiet}),
		mon:     &health.Monitor{Jobs: lossJobs{jobs: jobs}, Orchard: oc, Pauses: st, Log: quiet},
		expirer: &snooze.Expirer{Store: st, Orchard: p},
	}
}

func (w *snoozeWorld) post(path string) int {
	req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer operator")
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	return rec.Code
}

// run makes the Go call for one model process's request.
func (w *snoozeWorld) run(proc string) {
	ctx := context.Background()
	now := time.Now()
	switch proc {
	case "E", "F":
		_ = snooze.Engage(ctx, w.p, w.st, "mac1", snooze.Snooze{Until: now.Add(time.Hour), At: now}, now)
	case "W":
		_, _, _ = snooze.Wake(ctx, w.p, w.st, "mac1", now)
	case "R":
		w.post("/workers/mac1/resume")
	case "P":
		w.post("/workers/mac1/pause")
	case "H":
		w.mon.Tick(ctx)
	case "X":
		_ = w.expirer.Tick(ctx)
	default:
		w.t.Fatalf("unknown process %q", proc)
	}
}

// lapse makes the recorded snooze end now: the model's lapse step.
func (w *snoozeWorld) lapse() {
	e := w.st.Get("mac1")
	if e.Snooze == nil {
		w.t.Fatal("model lapsed a snooze grove has no record of")
	}
	sn := *e.Snooze
	sn.Until = time.Now().Add(-time.Second)
	if err := w.st.SetSnooze("mac1", sn); err != nil {
		w.t.Fatal(err)
	}
}

// observe is the Go state in the model's terms.
func (w *snoozeWorld) observe() (bit bool, reasons []string, expired bool) {
	w.p.mu.Lock()
	bit = w.p.paused
	w.p.mu.Unlock()
	e := w.st.Get("mac1")
	reasons = []string{}
	if e.Health != nil {
		reasons = append(reasons, "health")
	}
	if e.Operator != nil {
		reasons = append(reasons, "operator") // the model folds "external" into "operator"
	}
	if e.Snooze != nil {
		reasons = append(reasons, "snooze")
		expired = !e.Active(time.Now())
	}
	sort.Strings(reasons)
	return bit, reasons, expired
}

func replaySnoozeTrace(t *testing.T, tr itfTrace) (requests int) {
	t.Helper()
	if len(tr.states) == 0 {
		t.Fatalf("%s: empty trace", tr.file)
	}
	init := tr.states[0]
	w := newSnoozeWorld(t, init.bool("bit"))
	compare := func(i int, what string) {
		t.Helper()
		m := tr.states[i]
		bit, reasons, expired := w.observe()
		want := m.strSet("reasons")
		if want == nil {
			want = []string{}
		}
		wantExpired := m.bool("expired") && contains(want, "snooze")
		if bit != m.bool("bit") || !reflect.DeepEqual(reasons, want) || expired != wantExpired {
			t.Fatalf("%s state %d, after %s: grove has paused=%v reasons=%v lapsed=%v, the model has paused=%v reasons=%v lapsed=%v",
				tr.file, i, what, bit, reasons, expired, m.bool("bit"), want, wantExpired)
		}
	}
	compare(0, "init")

	active := "" // the process whose request is under way
	start := 0
	for i := 0; i+1 < len(tr.states); i++ {
		prev, next := tr.states[i], tr.states[i+1]
		pp, np := prev.strMap("pc"), next.strMap("pc")
		var moved []string
		for proc := range np {
			if np[proc] != pp[proc] {
				moved = append(moved, proc)
			}
		}
		switch {
		case len(moved) == 0 && !prev.bool("expired") && next.bool("expired"):
			if active != "" {
				t.Fatalf("%s state %d: a snooze lapsed in the middle of %s's request; traces must come from snoozeMBT", tr.file, i, active)
			}
			w.lapse()
			compare(i+1, "the snooze lapsing")
			continue
		case len(moved) == 0:
			continue // a stuttering step
		case len(moved) > 1:
			t.Fatalf("%s state %d: processes %v moved in one step", tr.file, i, moved)
		}
		proc := moved[0]
		if active == "" {
			if pp[proc] != startLabel[proc] {
				t.Fatalf("%s state %d: %s moved from %q without starting a request", tr.file, i, proc, pp[proc])
			}
			active, start = proc, i
		} else if proc != active {
			t.Fatalf("%s state %d: %s moved while %s's request was under way", tr.file, i, proc, active)
		}
		if !contains(endLabels[proc], np[proc]) {
			continue
		}
		// The request is complete in the model: make the same request of grove.
		failed := next.int("fails") < tr.states[start].int("fails")
		w.p.mu.Lock()
		w.p.failResume = 0
		if failed {
			w.p.failResume = 1
		}
		w.p.mu.Unlock()
		w.run(proc)
		w.p.mu.Lock()
		unused := w.p.failResume > 0
		w.p.failResume = 0
		w.p.mu.Unlock()
		if unused {
			t.Fatalf("%s states %d-%d: in the model %s's ResumeWorker call failed, but grove's %s made no ResumeWorker call",
				tr.file, start, i+1, proc, proc)
		}
		requests++
		compare(i+1, proc+"'s request (states "+itoa(start)+"-"+itoa(i+1)+")")
		active = ""
	}
	return requests
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func itoa(i int) string { return strconv.Itoa(i) }
