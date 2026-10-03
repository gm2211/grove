package fleet

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

type fakeNodeTokens struct {
	n       int
	tokens  []nomad.NodeToken
	revoked []string
}

func (f *fakeNodeTokens) CreateNodeToken(_ context.Context, vmName string) (nomad.NodeToken, error) {
	f.n++
	tok := nomad.NodeToken{
		AccessorID: fmt.Sprintf("acc-%d", f.n),
		SecretID:   fmt.Sprintf("secret-%d", f.n),
		Name:       nomad.NodeTokenPrefix + vmName,
		CreateTime: time.Now(),
	}
	f.tokens = append(f.tokens, nomad.NodeToken{AccessorID: tok.AccessorID, Name: tok.Name, CreateTime: tok.CreateTime})
	return tok, nil
}

func (f *fakeNodeTokens) ListNodeTokens(context.Context) ([]nomad.NodeToken, error) {
	return f.tokens, nil
}

func (f *fakeNodeTokens) RevokeNodeToken(_ context.Context, accessorID string) error {
	f.revoked = append(f.revoked, accessorID)
	return nil
}

func existingFrom(spec orchard.VMSpec) orchard.VM {
	return orchard.VM{
		Name:            spec.Name,
		Worker:          spec.Worker,
		Image:           spec.Image,
		CPU:             spec.CPU,
		Memory:          spec.Memory,
		DiskSize:        spec.DiskSize,
		RestartPolicy:   spec.RestartPolicy,
		StartupScript:   spec.StartupScript,
		ShutdownScript:  spec.ShutdownScript,
		ShutdownTimeout: spec.ShutdownTimeout,
		Labels:          map[string]string{LabelWorkerPin: spec.Worker},
	}
}

func TestNodeTokens_PlanHoldsNoSecretApplyMintsOne(t *testing.T) {
	tokens := &fakeNodeTokens{}
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{basePool()}}, Options: Options{NodeTokens: tokens}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 1 {
		t.Fatalf("want one create, got %+v", plan.Creates)
	}
	planned := plan.Creates[0].Spec
	if !strings.Contains(planned.ShutdownScript, nodeTokenSecretPlaceholder) || tokens.n != 0 {
		t.Fatalf("Plan must leave the token placeholder and mint nothing (minted %d):\n%s", tokens.n, planned.ShutdownScript)
	}
	if !strings.Contains(planned.StartupScript, "acl {\n  enabled = true\n}") {
		t.Errorf("startup script should turn on client ACLs:\n%s", planned.StartupScript)
	}

	if err := r.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	got := client.created[0]
	if tokens.n != 1 || tokens.tokens[0].Name != nomad.NodeTokenPrefix+planned.Name {
		t.Fatalf("want one token named for %s, got %+v", planned.Name, tokens.tokens)
	}
	for _, want := range []string{"NOMAD_TOKEN='secret-1'\nexport NOMAD_TOKEN\n", "# grove node token accessor acc-1 "} {
		if !strings.Contains(got.ShutdownScript, want) {
			t.Errorf("created shutdown script missing %q:\n%s", want, got.ShutdownScript)
		}
	}
	if strings.Contains(got.ShutdownScript, "@GROVE_") {
		t.Errorf("placeholder left in created shutdown script:\n%s", got.ShutdownScript)
	}
	if strings.Contains(got.StartupScript, "secret-1") {
		t.Error("node token leaked into the startup script; it belongs only in the shutdown script's env")
	}
	if strings.Index(got.ShutdownScript, "export NOMAD_TOKEN") > strings.Index(got.ShutdownScript, "nomad node drain") {
		t.Error("token must be exported before the drain")
	}

	// The created VM, token and all, is up to date on the next tick.
	client.vms = []orchard.VM{existingFrom(got)}
	plan, err = r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("a VM holding its own token must not read as drifted, got %+v", plan)
	}
}

func TestNodeTokens_TurningACLsOnDoesNotRecreateHealthyVMs(t *testing.T) {
	pool := basePool()
	name := VMName(pool.Name, "mac1", 0)
	before := (&Reconciler{}).vmSpec(pool, "mac1", name)

	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existingFrom(before)},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}, Options: Options{NodeTokens: &fakeNodeTokens{}}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("a VM made before node tokens should keep running until its next recycle, got %+v", plan)
	}
}

func TestNodeTokens_OtherShutdownChangesStillDrift(t *testing.T) {
	pool := basePool()
	r := &Reconciler{Options: Options{NodeTokens: &fakeNodeTokens{}}}
	desired := r.vmSpec(pool, "mac1", VMName(pool.Name, "mac1", 0))
	existing := existingFrom(desired)
	existing.ShutdownScript = strings.Replace(existing.ShutdownScript, "grove recycle", "something else", 1)
	if !specDrifted(desired, existing) {
		t.Fatal("a changed shutdown script outside the token block must still drift")
	}
}

func TestSweepNodeTokens_RevokesOnlyUnheldOldTokens(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	tokens := &fakeNodeTokens{tokens: []nomad.NodeToken{
		{AccessorID: "held", Name: nomad.NodeTokenPrefix + "linux-mac1-0", CreateTime: old},
		{AccessorID: "gone", Name: nomad.NodeTokenPrefix + "linux-mac2-0", CreateTime: old},
		{AccessorID: "fresh", Name: nomad.NodeTokenPrefix + "linux-mac3-0", CreateTime: now.Add(-time.Minute)},
	}}
	held := strings.NewReplacer(nodeTokenSecretPlaceholder, "'s'", nodeTokenAccessorPlaceholder, "held").Replace(nodeTokenBlock)
	client := &fakeOrchardClient{vms: []orchard.VM{{Name: "linux-mac1-0", ShutdownScript: "#!/bin/sh\n" + held}}}
	r := &Reconciler{Client: client, Options: Options{NodeTokens: tokens, Now: func() time.Time { return now }}}

	if err := r.SweepNodeTokens(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tokens.revoked, ",") != "gone" {
		t.Fatalf("revoked %v, want only the old token no VM holds", tokens.revoked)
	}
}

func TestSweepNodeTokens_NoopWithoutIssuer(t *testing.T) {
	r := &Reconciler{Client: &fakeOrchardClient{}}
	if err := r.SweepNodeTokens(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBuildStartupScript_ClientACLLandsInMetaFile(t *testing.T) {
	fakeBin := t.TempDir()
	metaPath := filepath.Join(t.TempDir(), "grove-meta.hcl")
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	writeExecutable("id", "#!/bin/sh\necho 502\n")
	writeExecutable("uname", "#!/bin/sh\necho Linux\n")
	// tee goes to a scratch meta file instead of /etc/nomad.d; everything else is a no-op.
	writeExecutable("sudo", `#!/bin/sh
[ "${1:-}" = "-n" ] && shift
case "${1:-}" in
  tee) if [ "${2:-}" = "-a" ]; then cat >> "$GROVE_TEST_META"; else cat > "$GROVE_TEST_META"; fi ;;
esac
exit 0
`)

	script := buildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0", Options{NodeTokens: &fakeNodeTokens{}})
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "GROVE_TEST_META="+metaPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("startup script failed: %v\n%s", err, output)
	}
	meta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "client {\n  meta {\n    pool = \"linux\"\n    host = \"mac1\"\n    vm = \"linux-mac1-0\"\n  }\n}\nacl {\n  enabled = true\n}\n"
	if string(meta) != want {
		t.Fatalf("meta file:\n%s\nwant:\n%s", meta, want)
	}
}

type failingNodeTokens struct{ fakeNodeTokens }

func (*failingNodeTokens) CreateNodeToken(context.Context, string) (nomad.NodeToken, error) {
	return nomad.NodeToken{}, fmt.Errorf("Permission denied")
}

func TestNodeTokens_MintFailureStillCreatesVM(t *testing.T) {
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{
		Client:  client,
		Spec:    &Spec{Pools: []Pool{basePool()}},
		Options: Options{NodeTokens: &failingNodeTokens{}, Logger: log.New(io.Discard, "", 0)},
	}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(context.Background(), plan); err != nil {
		t.Fatalf("a token problem must not stop VM creation: %v", err)
	}
	if len(client.created) != 1 {
		t.Fatalf("created %d VMs, want 1", len(client.created))
	}
	if s := client.created[0].ShutdownScript; strings.Contains(s, "@GROVE_") || strings.Contains(s, "NOMAD_TOKEN") {
		t.Errorf("token block left in a VM created without a token:\n%s", s)
	}
}
