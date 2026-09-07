package orchard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
)

func TestSplitToken(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantTok  string
	}{
		{"", "", ""},
		{"bare-token", "", "bare-token"},
		{"svc:secret", "svc", "secret"},
		{"svc:secret:with:colons", "svc", "secret:with:colons"},
	}

	for _, c := range cases {
		name, tok := splitToken(c.in)
		if name != c.wantName || tok != c.wantTok {
			t.Errorf("splitToken(%q) = (%q, %q), want (%q, %q)", c.in, name, tok, c.wantName, c.wantTok)
		}
	}
}

func TestVMToV1_PinsWorkerLabelFromHost(t *testing.T) {
	spec := VMSpec{
		Name:   "linux-mac1-0",
		Labels: map[string]string{"pool": "linux", "host": "mac1"},
	}

	vm := vmToV1(spec)

	if vm.Labels[v1.LabelWorkerName] != "mac1" {
		t.Errorf("want %s=%q, got %+v", v1.LabelWorkerName, "mac1", vm.Labels)
	}

	if vm.Labels["pool"] != "linux" || vm.Labels["host"] != "mac1" {
		t.Errorf("want original labels preserved, got %+v", vm.Labels)
	}
}

func TestVMToV1_StartupScript(t *testing.T) {
	vm := vmToV1(VMSpec{Name: "vm1", StartupScript: "echo hi"})

	if vm.StartupScript == nil || vm.StartupScript.ScriptContent != "echo hi" {
		t.Errorf("want startup script %q, got %+v", "echo hi", vm.StartupScript)
	}
}

func TestVMToV1_NoStartupScript(t *testing.T) {
	vm := vmToV1(VMSpec{Name: "vm1"})

	if vm.StartupScript != nil {
		t.Errorf("want nil startup script, got %+v", vm.StartupScript)
	}
}

func TestVMToV1_ShutdownScript(t *testing.T) {
	vm := vmToV1(VMSpec{
		Name:            "vm1",
		ShutdownScript:  "echo bye",
		ShutdownTimeout: 30 * time.Second,
	})

	if vm.ShutdownScript == nil || vm.ShutdownScript.ScriptContent != "echo bye" {
		t.Errorf("want shutdown script %q, got %+v", "echo bye", vm.ShutdownScript)
	}

	if vm.ShutdownScriptTimeoutSeconds != 30 {
		t.Errorf("want shutdown script timeout 30s, got %d", vm.ShutdownScriptTimeoutSeconds)
	}
}

func TestVMToV1_NoShutdownScript(t *testing.T) {
	vm := vmToV1(VMSpec{Name: "vm1"})

	if vm.ShutdownScript != nil {
		t.Errorf("want nil shutdown script, got %+v", vm.ShutdownScript)
	}

	if vm.ShutdownScriptTimeoutSeconds != 0 {
		t.Errorf("want zero shutdown script timeout, got %d", vm.ShutdownScriptTimeoutSeconds)
	}
}

func TestVMToV1_TTLNotYetWired(t *testing.T) {
	// gm2211/orchard's v1.VM has no TTL-shaped field yet (no ttl_seconds PR exists upstream,
	// open or merged), so a non-zero TTL on the spec has nothing to land on. This test just
	// documents that vmToV1 accepts such a spec without error and doesn't panic or otherwise
	// misbehave; there is no field to assert against.
	vm := vmToV1(VMSpec{Name: "vm1", TTL: time.Hour})

	if vm == nil || vm.Name != "vm1" {
		t.Fatalf("want vm1 produced despite TTL set, got %+v", vm)
	}
}

func TestVMFromV1_MapsAssignedResources(t *testing.T) {
	created := time.Now().Add(-time.Hour).Truncate(time.Second)

	v := v1.VM{
		Meta:           v1.Meta{Name: "vm1", CreatedAt: created},
		Image:          "img:latest",
		Status:         v1.VMStatusRunning,
		Worker:         "mac1",
		AssignedCPU:    4,
		AssignedMemory: 8192,
		RestartPolicy:  v1.RestartPolicyOnFailure,
		RestartCount:   2,
		Labels:         v1.Labels{"pool": "linux"},
	}

	out := vmFromV1(v)

	if out.Name != "vm1" || out.Image != "img:latest" || out.Worker != "mac1" {
		t.Fatalf("unexpected mapping: %+v", out)
	}

	if out.CPU != 4 || out.Memory != 8192 {
		t.Errorf("want assigned cpu/memory 4/8192, got %d/%d", out.CPU, out.Memory)
	}

	if out.Status != string(v1.VMStatusRunning) || out.RestartPolicy != string(v1.RestartPolicyOnFailure) {
		t.Errorf("unexpected status/restart policy: %+v", out)
	}

	if !out.CreatedAt.Equal(created) {
		t.Errorf("want createdAt %v, got %v", created, out.CreatedAt)
	}

	if out.Labels["pool"] != "linux" {
		t.Errorf("want label pool=linux, got %+v", out.Labels)
	}
}

func TestWorkerFromV1_Offline(t *testing.T) {
	stale := v1.Worker{Name: "mac1", LastSeen: time.Now().Add(-time.Minute)}
	fresh := v1.Worker{Name: "mac2", LastSeen: time.Now()}

	if !workerFromV1(stale).Offline {
		t.Errorf("want worker last seen 1m ago to be offline")
	}

	if workerFromV1(fresh).Offline {
		t.Errorf("want worker last seen just now to be online")
	}
}

// fakeController is a minimal in-memory implementation of the slice of the Orchard v1 HTTP API
// that internal/orchard/client.go talks to, enough to exercise Client end-to-end without a real
// Orchard controller.
type fakeController struct {
	mu      sync.Mutex
	workers map[string]v1.Worker
	vms     map[string]v1.VM
}

func newFakeController() (*fakeController, http.Handler) {
	fc := &fakeController{
		workers: map[string]v1.Worker{},
		vms:     map[string]v1.VM{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", fc.handle)

	return fc, mux
}

func (fc *fakeController) handle(w http.ResponseWriter, r *http.Request) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	path := r.URL.Path

	switch {
	case path == "/v1" || path == "/v1/":
		w.WriteHeader(http.StatusOK)
	case path == "/v1/workers" && r.Method == http.MethodGet:
		writeJSON(w, valuesOf(fc.workers))
	case strings.HasPrefix(path, "/v1/workers/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "/v1/workers/")
		worker, ok := fc.workers[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, worker)
	case strings.HasPrefix(path, "/v1/workers/") && r.Method == http.MethodPut:
		name := strings.TrimPrefix(path, "/v1/workers/")
		var worker v1.Worker
		_ = json.NewDecoder(r.Body).Decode(&worker)
		fc.workers[name] = worker
		writeJSON(w, worker)
	case path == "/v1/vms" && r.Method == http.MethodGet:
		writeJSON(w, valuesOf(fc.vms))
	case path == "/v1/vms" && r.Method == http.MethodPost:
		var vm v1.VM
		_ = json.NewDecoder(r.Body).Decode(&vm)
		vm.Status = v1.VMStatusRunning
		fc.vms[vm.Name] = vm
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(path, "/v1/vms/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "/v1/vms/")
		vm, ok := fc.vms[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, vm)
	case strings.HasPrefix(path, "/v1/vms/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/v1/vms/")
		delete(fc.vms, name)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func valuesOf[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}

	return out
}

func TestClient_EndToEnd(t *testing.T) {
	fc, handler := newFakeController()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	c, err := New(ts.URL, "svc:token")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// Seed a worker directly, then verify ListWorkers/PauseWorker/ResumeWorker.
	fc.mu.Lock()
	fc.workers["mac1"] = v1.Worker{Name: "mac1", LastSeen: time.Now()}
	fc.mu.Unlock()

	workers, err := c.ListWorkers(ctx)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}

	if len(workers) != 1 || workers[0].Name != "mac1" || workers[0].Offline {
		t.Fatalf("unexpected workers: %+v", workers)
	}

	if err := c.PauseWorker(ctx, "mac1"); err != nil {
		t.Fatalf("PauseWorker: %v", err)
	}

	workers, err = c.ListWorkers(ctx)
	if err != nil {
		t.Fatalf("ListWorkers after pause: %v", err)
	}

	if len(workers) != 1 || !workers[0].SchedulingPaused {
		t.Fatalf("want worker paused, got %+v", workers)
	}

	if err := c.ResumeWorker(ctx, "mac1"); err != nil {
		t.Fatalf("ResumeWorker: %v", err)
	}

	workers, err = c.ListWorkers(ctx)
	if err != nil {
		t.Fatalf("ListWorkers after resume: %v", err)
	}

	if len(workers) != 1 || workers[0].SchedulingPaused {
		t.Fatalf("want worker resumed, got %+v", workers)
	}

	created, err := c.CreateVM(ctx, VMSpec{
		Name:   "linux-mac1-0",
		Image:  "ghcr.io/example/linux:latest",
		Labels: map[string]string{"pool": "linux", "host": "mac1"},
	})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}

	if created.Name != "linux-mac1-0" || created.Status != string(v1.VMStatusRunning) {
		t.Fatalf("unexpected created VM: %+v", created)
	}

	got, err := c.GetVM(ctx, "linux-mac1-0")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}

	if got.Name != "linux-mac1-0" {
		t.Fatalf("unexpected GetVM result: %+v", got)
	}

	vms, err := c.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}

	if len(vms) != 1 {
		t.Fatalf("want 1 vm, got %d", len(vms))
	}

	if err := c.DeleteVM(ctx, "linux-mac1-0"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}

	vms, err = c.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs after delete: %v", err)
	}

	if len(vms) != 0 {
		t.Fatalf("want 0 vms after delete, got %d", len(vms))
	}
}
