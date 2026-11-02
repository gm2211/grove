package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/pki"
)

type introCall struct {
	name, pool string
	ttl        time.Duration
}

type fakeNodeIntros struct {
	calls []introCall
	err   error
}

func (f *fakeNodeIntros) CreateIntroToken(_ context.Context, nodeName, nodePool string, ttl time.Duration) (string, error) {
	f.calls = append(f.calls, introCall{nodeName, nodePool, ttl})
	if f.err != nil {
		return "", f.err
	}
	return fmt.Sprintf("intro-jwt-%d-for-%s", len(f.calls), nodeName), nil
}

type fakeNodeCerts struct {
	issued []string
}

func (f *fakeNodeCerts) IssueNode(vmName string, now time.Time, validity time.Duration) (pki.NodeCert, error) {
	f.issued = append(f.issued, vmName)
	n := len(f.issued)
	return pki.NodeCert{
		Cert:     fmt.Sprintf("-----BEGIN CERTIFICATE-----\nCERT-%d-%s\n-----END CERTIFICATE-----\n", n, vmName),
		Key:      fmt.Sprintf("-----BEGIN EC PRIVATE KEY-----\nKEY-%d-%s\n-----END EC PRIVATE KEY-----\n", n, vmName),
		NotAfter: now.Add(validity),
	}, nil
}

type fakeNodeList struct {
	nodes []nomad.NodeSummary
}

func (f *fakeNodeList) ListNodeSummaries(context.Context) ([]nomad.NodeSummary, error) {
	return f.nodes, nil
}

type fakeNodePools struct {
	ensured []string
}

func (f *fakeNodePools) EnsureNodePool(_ context.Context, name, _ string) error {
	f.ensured = append(f.ensured, name)
	return nil
}

var testNodeTLS = &NodeTLS{CA: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"}

func TestNodeTLS_SpecHoldsNoNodeSecrets(t *testing.T) {
	intros := &fakeNodeIntros{}
	certs := &fakeNodeCerts{}
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{basePool()}}, Options: Options{
		NodeTokens: &fakeNodeTokens{}, NodeTLS: testNodeTLS, NodeIntros: intros, NodeCerts: certs,
		Nodes: &fakeNodeList{}, NodePools: &fakeNodePools{},
	}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(client.created) != 1 {
		t.Fatalf("want one create, got %+v", client.created)
	}
	if len(intros.calls) != 0 || len(certs.issued) != 0 {
		t.Fatalf("creating a VM must mint nothing for it (tokens %d, certificates %d)", len(intros.calls), len(certs.issued))
	}
	got := client.created[0].StartupScript
	for _, want := range []string{
		"\nCA\n",
		"verify_server_hostname = true",
		`name = "$grove_vm"`,
		`node_pool = "$grove_pool"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("created startup script missing %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"PRIVATE KEY", "intro_token", "@GROVE_"} {
		if strings.Contains(got, leak) {
			t.Errorf("created startup script holds %q:\n%s", leak, got)
		}
	}
	// Nomad stays down until the enrolment has landed.
	if i, j := strings.Index(got, nomadEnrolWaitMarker), strings.Index(got, "systemctl restart nomad"); i < 0 || j < 0 || i > j {
		t.Errorf("the enrolment wait must come before the Nomad restart (%d, %d)", i, j)
	}

	client.vms = []orchard.VM{existingFrom(client.created[0])}
	plan, err = r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("the created VM must be up to date, got %+v", plan)
	}
}

func TestNodeTLS_TurningTLSOnRecreatesVMsThatCannotRegister(t *testing.T) {
	pool := basePool()
	name := VMName(pool.Name, "mac1", 0)
	before := (&Reconciler{Options: Options{NodeTokens: &fakeNodeTokens{}}}).vmSpec(pool, "mac1", name)

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existingFrom(before)},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}, Options: Options{
		NodeTokens: &fakeNodeTokens{}, NodeTLS: testNodeTLS,
	}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 1 {
		t.Fatalf("a VM without TLS can't join a strict-introduction server and must be recreated, got %+v", plan)
	}
}

func TestNodeTLS_NewCARecreatesVMs(t *testing.T) {
	pool := basePool()
	name := VMName(pool.Name, "mac1", 0)
	before := (&Reconciler{Options: Options{NodeTLS: testNodeTLS}}).vmSpec(pool, "mac1", name)
	rotated := NodeTLS{CA: "-----BEGIN CERTIFICATE-----\nNEWCA\n-----END CERTIFICATE-----\n"}
	r := &Reconciler{Options: Options{NodeTLS: &rotated}}
	if !specDrifted(r.vmSpec(pool, "mac1", name), existingFrom(before)) {
		t.Fatal("a VM trusting the old CA must read as drifted")
	}
}

func TestNodeTLS_SealedPoolsGetNone(t *testing.T) {
	pool := basePool()
	pool.Network = NetworkIsolated
	script := buildStartupScript(pool, "mac1", "linux-mac1-0", Options{NodeTLS: testNodeTLS})
	if strings.Contains(script, "grove:nomad-tls") || strings.Contains(script, nomadEnrolWaitMarker) {
		t.Fatalf("a sealed VM runs no Nomad client and must get no Nomad TLS config:\n%s", script)
	}
}

// guestDirs are a fake VM's Nomad config and state directories, standing in for /etc/nomad.d and
// /opt/nomad/data in the generated shell.
type guestDirs struct {
	nomad, state string
}

func newGuestDirs(t *testing.T) guestDirs {
	root := t.TempDir()
	return guestDirs{nomad: filepath.Join(root, "nomad.d"), state: filepath.Join(root, "data")}
}

func (g guestDirs) rewrite(script string) string {
	return strings.NewReplacer(
		"/usr/local/etc/nomad.d", g.nomad,
		"/etc/nomad.d", g.nomad,
		"/opt/nomad/data", g.state,
	).Replace(script)
}

// runEnrol runs enrolGuestScript the way the exec does, with stdin, and pkill recorded in hups.
func (g guestDirs) runEnrol(t *testing.T, stdin string) (int, string) {
	t.Helper()
	script := "pkill() { echo \"$*\" >>" + shQuote(filepath.Join(g.nomad, "hups")) + "; }\n" + g.rewrite(enrolGuestScript)
	cmd := guestCmd(t, "", "-c", script)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func (g guestDirs) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.nomad, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestNodeTLS_StartupAndEnrolmentWriteRootOnlyFiles runs the startup script's TLS block, then the
// enrolment exec, then the startup script's wait, in a shell, and checks what lands on disk.
func TestNodeTLS_StartupAndEnrolmentWriteRootOnlyFiles(t *testing.T) {
	g := newGuestDirs(t)
	prelude := "set -eu\ngrove_priv() { \"$@\"; }\ngrove_vm='linux-mac1-0'\ngrove_pool='linux'\ngrove_meta_file=" +
		shQuote(filepath.Join(g.nomad, "grove-meta.hcl")) + "\nmkdir -p " + shQuote(g.nomad) + "\n"
	if out, err := guestCmd(t, "", "-c", prelude+nomadTLSScript(*testNodeTLS)).CombinedOutput(); err != nil {
		t.Fatalf("TLS block failed: %v\n%s", err, out)
	}

	cert, _ := (&fakeNodeCerts{}).IssueNode("linux-mac1-0", time.Now(), time.Hour)
	if code, out := g.runEnrol(t, enrolPayload("the-jwt", cert)); code != 0 {
		t.Fatalf("enrolment exited %d:\n%s", code, out)
	}
	// The startup script carries on once enrolled.
	if out, err := guestCmd(t, "", "-c", prelude+"grove_nomad_tls="+shQuote(filepath.Join(g.nomad, "tls"))+"\n"+nomadEnrolWaitScript+"echo past-wait\n").CombinedOutput(); err != nil || !strings.Contains(string(out), "past-wait") {
		t.Fatalf("enrolment wait: %v\n%s", err, out)
	}

	if info, err := os.Stat(filepath.Join(g.nomad, "tls")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("tls dir: %v, %v; want mode 0700", info, err)
	}
	for name, want := range map[string]string{"ca.pem": testNodeTLS.CA, "node.pem": cert.Cert, "node-key.pem": cert.Key} {
		if got := g.read(t, filepath.Join("tls", name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		if info, _ := os.Stat(filepath.Join(g.nomad, "tls", name)); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(g.nomad, "tls")); len(entries) != 4 {
		t.Errorf("tls dir holds %v, want ca.pem, node.pem, node-key.pem and enrolled only", entries)
	}
	hcl := g.read(t, "grove-tls.hcl")
	for _, want := range []string{
		`name = "linux-mac1-0"`,
		`node_pool = "linux"`,
		`ca_file                = "` + filepath.Join(g.nomad, "tls", "ca.pem") + `"`,
		`cert_file              = "` + filepath.Join(g.nomad, "tls", "node.pem") + `"`,
		`key_file               = "` + filepath.Join(g.nomad, "tls", "node-key.pem") + `"`,
		"rpc                    = true",
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("grove-tls.hcl missing %q:\n%s", want, hcl)
		}
	}
	tokenPath := filepath.Join(g.state, "client", "intro_token.jwt")
	data, err := os.ReadFile(tokenPath)
	if err != nil || string(data) != "the-jwt" {
		t.Fatalf("intro token = %q, %v; want the-jwt with no newline", data, err)
	}
	if info, _ := os.Stat(tokenPath); info.Mode().Perm() != 0o600 {
		t.Errorf("intro token mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(g.nomad, "hups")); err == nil {
		t.Error("a first enrolment must not signal Nomad: the startup script restarts it")
	}
}

func TestEnrolGuestScript_RefusesABadPayload(t *testing.T) {
	g := newGuestDirs(t)
	for name, stdin := range map[string]string{
		"no header":      "\n\n-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n",
		"no key":         enrolPayloadHeader + "\n\n-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n",
		"nothing at all": "",
	} {
		if code, out := g.runEnrol(t, stdin); code == 0 {
			t.Errorf("%s: enrolment succeeded:\n%s", name, out)
		}
		if _, err := os.Stat(filepath.Join(g.nomad, "tls", "enrolled")); err == nil {
			t.Fatalf("%s: a failed enrolment marked the VM enrolled", name)
		}
	}
}

// enrolFixture is a fleet with one running shared VM whose execs run the real guest script.
type enrolFixture struct {
	t       *testing.T
	r       *Reconciler
	client  *fakeOrchardClient
	intros  *fakeNodeIntros
	certs   *fakeNodeCerts
	nodes   *fakeNodeList
	pools   *fakeNodePools
	guest   guestDirs
	now     time.Time
	execs   []string // each exec's argv, joined
	execErr int      // when set, every exec exits with it
	state   EnrolState
}

func newEnrolFixture(t *testing.T) *enrolFixture {
	f := &enrolFixture{
		t: t, intros: &fakeNodeIntros{}, certs: &fakeNodeCerts{}, nodes: &fakeNodeList{}, pools: &fakeNodePools{},
		guest: newGuestDirs(t), now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	pool := basePool()
	name := VMName(pool.Name, "mac1", 0)
	opts := Options{
		NodeTLS: testNodeTLS, NodeIntros: f.intros, NodeCerts: f.certs, Nodes: f.nodes, NodePools: f.pools,
		Now: func() time.Time { return f.now },
	}
	vm := existingFrom((&Reconciler{Options: opts}).vmSpec(pool, "mac1", name))
	vm.UID, vm.Status = "uid-1", "running"
	f.client = &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}, vms: []orchard.VM{vm}}
	f.client.exec = func(_ string, command []string, stdin string) (int, string) {
		f.execs = append(f.execs, strings.Join(command, " "))
		if f.execErr != 0 {
			return f.execErr, "boom"
		}
		return f.guest.runEnrol(t, stdin)
	}
	f.r = &Reconciler{Client: f.client, Spec: &Spec{Pools: []Pool{pool}}, Options: opts}
	return f
}

func (f *enrolFixture) enrol() error {
	f.t.Helper()
	return f.r.Enrol(context.Background(), &f.state)
}

func (f *enrolFixture) register(status string) {
	f.nodes.nodes = []nomad.NodeSummary{{ID: "n1", Name: "linux-mac1-0", Status: status, NodePool: "linux", Eligible: true}}
}

func (f *enrolFixture) hups() string {
	data, _ := os.ReadFile(filepath.Join(f.guest.nomad, "hups"))
	return string(data)
}

func TestEnrol_FirstBootGetsACertificateAndAPoolBoundToken(t *testing.T) {
	f := newEnrolFixture(t)
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if len(f.execs) != 1 {
		t.Fatalf("want one enrolment exec, got %d", len(f.execs))
	}
	if want := (introCall{"linux-mac1-0", "linux", introTokenTTL}); len(f.intros.calls) != 1 || f.intros.calls[0] != want {
		t.Fatalf("intro tokens = %+v, want one %+v", f.intros.calls, want)
	}
	if len(f.pools.ensured) != 1 || f.pools.ensured[0] != "linux" {
		t.Fatalf("node pools ensured = %v, want [linux] before the token", f.pools.ensured)
	}
	if got := f.guest.read(t, "tls/node.pem"); !strings.Contains(got, "CERT-1-linux-mac1-0") {
		t.Fatalf("node.pem = %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(f.guest.state, "client", "intro_token.jwt"))
	if string(data) != "intro-jwt-1-for-linux-mac1-0" {
		t.Fatalf("intro token = %q", data)
	}
	// Secrets travel on stdin only: argv is logged by sudo and visible in ps.
	for _, secret := range []string{"intro-jwt", "KEY-1", "CERT-1"} {
		if strings.Contains(f.execs[0], secret) {
			t.Fatalf("exec argv holds %q: %s", secret, f.execs[0])
		}
	}
	if !strings.HasPrefix(f.execs[0], "sudo -n /bin/sh -c ") {
		t.Fatalf("exec argv = %s", f.execs[0])
	}

	// Waiting for the client to register: nothing more is minted, nor once it has.
	f.now = f.now.Add(time.Minute)
	if err := f.enrol(); err != nil || len(f.execs) != 1 {
		t.Fatalf("enrolled again while the client registers (%d execs, %v)", len(f.execs), err)
	}
	f.register("ready")
	f.now = f.now.Add(time.Hour)
	if err := f.enrol(); err != nil || len(f.execs) != 1 {
		t.Fatalf("enrolled a registered VM again (%d execs, %v)", len(f.execs), err)
	}
}

func TestEnrol_RenewsTheCertificateHalfwayWithoutAToken(t *testing.T) {
	f := newEnrolFixture(t)
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	f.register("ready")
	f.now = f.now.Add(pki.NodeCertValidity - nodeCertRenewBefore - time.Minute)
	if err := f.enrol(); err != nil || len(f.execs) != 1 {
		t.Fatalf("renewed too early (%d execs, %v)", len(f.execs), err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if len(f.execs) != 2 || len(f.intros.calls) != 1 {
		t.Fatalf("want a renewal exec and no new token, got %d execs and %d tokens", len(f.execs), len(f.intros.calls))
	}
	if got := f.guest.read(t, "tls/node.pem"); !strings.Contains(got, "CERT-2-") {
		t.Fatalf("node.pem not renewed: %q", got)
	}
	if got := f.guest.read(t, "tls/node-key.pem"); !strings.Contains(got, "KEY-2-") {
		t.Fatalf("node-key.pem not renewed: %q", got)
	}
	if got := f.hups(); got != "-HUP -x nomad\n" {
		t.Fatalf("a renewal must SIGHUP Nomad once, got %q", got)
	}
	// The token from first boot is not handed out again.
	data, _ := os.ReadFile(filepath.Join(f.guest.state, "client", "intro_token.jwt"))
	if string(data) != "intro-jwt-1-for-linux-mac1-0" {
		t.Fatalf("intro token = %q", data)
	}
}

func TestEnrol_AVMThatNeverRegisteredGetsAFreshToken(t *testing.T) {
	f := newEnrolFixture(t)
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(introTokenTTL + time.Minute)
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if len(f.intros.calls) != 2 {
		t.Fatalf("want a second token once the first expired unused, got %d", len(f.intros.calls))
	}
}

func TestEnrol_ARecreatedVMIsEnrolledAgainDespiteItsPredecessorsNode(t *testing.T) {
	f := newEnrolFixture(t)
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	f.register("down")
	f.client.vms[0].UID = "uid-2"
	if err := os.RemoveAll(f.guest.nomad); err != nil { // a new VM has a new disk
		t.Fatal(err)
	}
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if len(f.intros.calls) != 2 || len(f.execs) != 2 {
		t.Fatalf("the new VM must get its own token (%d tokens, %d execs)", len(f.intros.calls), len(f.execs))
	}
	if f.hups() != "" {
		t.Fatal("a first enrolment must not signal Nomad")
	}
}

func TestEnrol_SkipsVMsThatAreNotWaitingForIt(t *testing.T) {
	for name, edit := range map[string]func(*orchard.VM){
		"pending": func(vm *orchard.VM) { vm.Status = "pending" },
		"no wait": func(vm *orchard.VM) {
			vm.StartupScript = strings.ReplaceAll(vm.StartupScript, nomadEnrolWaitMarker, "")
		},
		"foreign": func(vm *orchard.VM) { vm.Name = "nodash" },
	} {
		f := newEnrolFixture(t)
		edit(&f.client.vms[0])
		if err := f.enrol(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(f.execs) != 0 || len(f.intros.calls) != 0 || len(f.certs.issued) != 0 {
			t.Fatalf("%s: enrolled (%d execs)", name, len(f.execs))
		}
	}
}

func TestEnrol_AFailedExecIsRetriedNextTick(t *testing.T) {
	f := newEnrolFixture(t)
	f.execErr = 2
	if err := f.enrol(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want the exec's failure reported, got %v", err)
	}
	f.execErr = 0
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if len(f.execs) != 2 {
		t.Fatalf("want a retry, got %d execs", len(f.execs))
	}
}

func TestEnrol_NoTLSIsANoOpAndHalfConfiguredIsAnError(t *testing.T) {
	f := newEnrolFixture(t)
	f.r.Options.NodeTLS = nil
	if err := f.enrol(); err != nil || len(f.execs) != 0 {
		t.Fatalf("without NodeTLS: %v, %d execs", err, len(f.execs))
	}
	f.r.Options.NodeTLS = testNodeTLS
	f.r.Options.NodeCerts = nil
	if err := f.enrol(); err == nil {
		t.Fatal("NodeTLS without a certificate issuer must fail loudly: no VM could ever register")
	}
}

func TestEnrol_OnlyVMsTheReconcilerWouldHaveMade(t *testing.T) {
	for name, edit := range map[string]func(*enrolFixture){
		"other image":  func(f *enrolFixture) { f.client.vms[0].Image = "ghcr.io/evil/linux:latest" },
		"other worker": func(f *enrolFixture) { f.client.vms[0].Worker = "mac2" },
		"unknown pool": func(f *enrolFixture) { f.client.vms[0].Name = "gpu-mac1-0" },
		"sealed pool":  func(f *enrolFixture) { f.r.Spec.Pools[0].Network = NetworkIsolated },
		"host dirs": func(f *enrolFixture) {
			f.client.vms[0].HostDirs = []orchard.HostDir{{Name: "home", Path: "/Users/me"}}
		},
	} {
		f := newEnrolFixture(t)
		edit(f)
		if err := f.enrol(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(f.execs) != 0 || len(f.intros.calls) != 0 {
			t.Errorf("%s: a VM grove didn't plan was enrolled", name)
		}
	}
}

func TestEnrol_ANodeTakenOutOfServiceDoesNotCountAsTheVMs(t *testing.T) {
	f := newEnrolFixture(t)
	// The roll call drained a duplicate claiming this name; the rebuilt VM still needs a token.
	f.nodes.nodes = []nomad.NodeSummary{{ID: "twin", Name: "linux-mac1-0", Status: "ready", NodePool: "linux", Eligible: false}}
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if len(f.intros.calls) != 1 {
		t.Fatalf("want a token for the rebuilt VM, got %d", len(f.intros.calls))
	}
	// A draining node is still there: no fresh token every half hour while it drains.
	f.now = f.now.Add(introTokenTTL + time.Minute)
	if err := f.enrol(); err != nil || len(f.intros.calls) != 1 {
		t.Fatalf("re-enrolled a VM whose node is present (%d tokens, %v)", len(f.intros.calls), err)
	}
}

func TestEnrol_FailureNamesTheVMsPoolForPoolHealth(t *testing.T) {
	f := newEnrolFixture(t)
	f.r.Options.NodePools = failingNodePools{}
	err := f.enrol()
	if err == nil || !strings.Contains(err.Error(), "enrol linux-mac1-0: nomad: read node pool linux: 403") {
		t.Fatalf("err = %v", err)
	}
	var h PoolHealth
	h.ObserveEnrol(err)
	if r := h.UnplaceableReason("linux"); !strings.Contains(r, "pool linux has no ready node: enrollment failing: enrol linux-mac1-0") {
		t.Fatalf("reason = %q", r)
	}
}

type failingNodePools struct{}

func (failingNodePools) EnsureNodePool(context.Context, string, string) error {
	return errors.New("nomad: read node pool linux: 403")
}

// TestNodeTLS_ShutdownSkipsDrainWhenNeverEnrolled: a shared VM the control plane never enrolled
// has no Nomad node, so its shutdown script must end before it asks Nomad anything. Otherwise
// the alloc poll waits on a client still retrying registration and holds the VM's deletion (and
// the Orchard worker's sync loop) for the whole ShutdownTimeout.
func TestNodeTLS_ShutdownSkipsDrainWhenNeverEnrolled(t *testing.T) {
	pool := basePool()
	name := VMName(pool.Name, "mac1", 0)

	spec := (&Reconciler{Options: Options{NodeTokens: &fakeNodeTokens{}, NodeTLS: testNodeTLS}}).vmSpec(pool, "mac1", name)
	exit := strings.Index(spec.ShutdownScript, "# grove:unenrolled-exit")
	nomad := strings.Index(spec.ShutdownScript, "nomad node status")
	if exit < 0 || nomad < 0 || exit > nomad {
		t.Fatalf("the never-enrolled exit must come before the alloc poll:\n%s", spec.ShutdownScript)
	}
	if !strings.HasPrefix(spec.ShutdownScript, "#!/bin/sh\nset -eu\n\n# grove:unenrolled-exit") {
		t.Fatalf("the never-enrolled exit must follow the script header:\n%s", spec.ShutdownScript)
	}
	if out, err := guestCmd(t, "", "-n", "-c", spec.ShutdownScript).CombinedOutput(); err != nil {
		t.Fatalf("shutdown script doesn't parse: %v\n%s", err, out)
	}

	// Without TLS there's no enrolment, and a sealed VM runs no Nomad client
	plain := (&Reconciler{Options: Options{NodeTokens: &fakeNodeTokens{}}}).vmSpec(pool, "mac1", name)
	sealedPool := basePool()
	sealedPool.Network = NetworkIsolated
	sealed := (&Reconciler{Options: Options{NodeTLS: testNodeTLS}}).vmSpec(sealedPool, "mac1", name)
	for label, script := range map[string]string{"no TLS": plain.ShutdownScript, "sealed": sealed.ShutdownScript} {
		if strings.Contains(script, "grove:unenrolled-exit") {
			t.Errorf("%s: unexpected never-enrolled exit:\n%s", label, script)
		}
	}
}
