package fleet

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
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
	return "intro-jwt-for-" + nodeName, nil
}

var testNodeTLS = &NodeTLS{
	CA:   "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
	Cert: "-----BEGIN CERTIFICATE-----\nNODE\n-----END CERTIFICATE-----\n",
	Key:  "-----BEGIN EC PRIVATE KEY-----\nKEY\n-----END EC PRIVATE KEY-----\n",
}

func TestNodeTLS_PlanHoldsNoTokenApplyMintsOneForThatVM(t *testing.T) {
	intros := &fakeNodeIntros{}
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{basePool()}}, Options: Options{
		NodeTokens: &fakeNodeTokens{}, NodeTLS: testNodeTLS, NodeIntros: intros,
	}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 1 {
		t.Fatalf("want one create, got %+v", plan.Creates)
	}
	planned := plan.Creates[0].Spec
	if !strings.Contains(planned.StartupScript, nomadIntroTokenPlaceholder) || len(intros.calls) != 0 {
		t.Fatalf("Plan must leave the intro placeholder and mint nothing (minted %d)", len(intros.calls))
	}

	if err := r.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(intros.calls) != 1 || intros.calls[0] != (introCall{planned.Name, "", introTokenTTL}) {
		t.Fatalf("want one token bound to %s for %v, got %+v", planned.Name, introTokenTTL, intros.calls)
	}
	got := client.created[0].StartupScript
	for _, want := range []string{
		"printf '%s' 'intro-jwt-for-" + planned.Name + "' | grove_priv tee /opt/nomad/data/client/intro_token.jwt",
		"\nNODE\n", "\nKEY\n", "\nCA\n",
		"verify_server_hostname = true",
		`name = "$grove_vm"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("created startup script missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "@GROVE_") {
		t.Errorf("placeholder left in created startup script:\n%s", got)
	}
	// TLS and the token are in place before Nomad restarts onto the new config.
	if i, j := strings.Index(got, "grove:nomad-intro end"), strings.Index(got, "systemctl restart nomad"); i < 0 || j < 0 || i > j {
		t.Errorf("intro token must be written before the Nomad restart (%d, %d)", i, j)
	}

	// The created VM, token and all, is up to date on the next tick.
	client.vms = []orchard.VM{existingFrom(client.created[0])}
	plan, err = r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("a VM holding its own intro token must not read as drifted, got %+v", plan)
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
		NodeTokens: &fakeNodeTokens{}, NodeTLS: testNodeTLS, NodeIntros: &fakeNodeIntros{},
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
	rotated := *testNodeTLS
	rotated.CA = "-----BEGIN CERTIFICATE-----\nNEWCA\n-----END CERTIFICATE-----\n"
	r := &Reconciler{Options: Options{NodeTLS: &rotated}}
	if !specDrifted(r.vmSpec(pool, "mac1", name), existingFrom(before)) {
		t.Fatal("a VM trusting the old CA must read as drifted")
	}
}

func TestNodeTLS_MintFailureCreatesNothing(t *testing.T) {
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{basePool()}}, Options: Options{
		NodeTLS: testNodeTLS, NodeIntros: &fakeNodeIntros{err: errors.New("Permission denied")},
	}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(context.Background(), plan); err == nil {
		t.Fatal("a VM that could never register must not be created")
	}
	if len(client.created) != 0 {
		t.Fatalf("created %d VMs, want 0", len(client.created))
	}
}

func TestNodeTLS_SealedPoolsGetNone(t *testing.T) {
	pool := basePool()
	pool.Network = NetworkIsolated
	script := buildStartupScript(pool, "mac1", "linux-mac1-0", Options{NodeTLS: testNodeTLS, NodeIntros: &fakeNodeIntros{}})
	if strings.Contains(script, "grove:nomad-tls") || strings.Contains(script, nomadIntroTokenPlaceholder) {
		t.Fatalf("a sealed VM runs no Nomad client and must get no Nomad TLS material:\n%s", script)
	}
}

// TestNodeTLS_ScriptWritesRootOnlyFiles runs the generated blocks in a shell, with the config and
// state directories moved into a scratch dir, and checks what lands on disk.
func TestNodeTLS_ScriptWritesRootOnlyFiles(t *testing.T) {
	root := t.TempDir()
	nomadDir := filepath.Join(root, "nomad.d")
	stateDir := filepath.Join(root, "data")
	block := nomadTLSScript(*testNodeTLS) + strings.ReplaceAll(
		strings.ReplaceAll(nomadIntroScript, "/opt/nomad/data", stateDir),
		nomadIntroTokenPlaceholder, shQuote("the-jwt"))
	script := "set -eu\ngrove_priv() { \"$@\"; }\ngrove_vm='linux-mac1-0'\ngrove_meta_file=" +
		shQuote(filepath.Join(nomadDir, "grove-meta.hcl")) + "\nmkdir -p " + shQuote(nomadDir) + "\n" + block
	if out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	if info, err := os.Stat(filepath.Join(nomadDir, "tls")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("tls dir mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	for name, want := range map[string]string{"ca.pem": testNodeTLS.CA, "node.pem": testNodeTLS.Cert, "node-key.pem": testNodeTLS.Key} {
		path := filepath.Join(nomadDir, "tls", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", name, data, want)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
	hcl, err := os.ReadFile(filepath.Join(nomadDir, "grove-tls.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "linux-mac1-0"`,
		`ca_file                = "` + filepath.Join(nomadDir, "tls", "ca.pem") + `"`,
		`key_file               = "` + filepath.Join(nomadDir, "tls", "node-key.pem") + `"`,
		"rpc                    = true",
	} {
		if !strings.Contains(string(hcl), want) {
			t.Errorf("grove-tls.hcl missing %q:\n%s", want, hcl)
		}
	}
	tokenPath := filepath.Join(stateDir, "client", "intro_token.jwt")
	data, err := os.ReadFile(tokenPath)
	if err != nil || string(data) != "the-jwt" {
		t.Fatalf("intro token = %q, %v; want the-jwt with no newline", data, err)
	}
	if info, _ := os.Stat(tokenPath); info.Mode().Perm() != 0o600 {
		t.Errorf("intro token mode = %v, want 0600", info.Mode().Perm())
	}
}
