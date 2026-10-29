package fleet

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/orchard"
)

func macosPool() Pool {
	pool := basePool()
	pool.Name = MacOSPool
	pool.Image = "ghcr.io/example/macos:latest"
	return pool
}

var hexPassword = regexp.MustCompile(`^[0-9a-f]{48}$`)

func TestGuestPassword_EachVMGetsItsOwnAndOrchardTheSame(t *testing.T) {
	pool := macosPool()
	pool.PerWorker = 2
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 2 {
		t.Fatalf("want two creates, got %+v", plan.Creates)
	}
	for _, c := range plan.Creates {
		if !strings.Contains(c.Spec.StartupScript, guestPasswordPlaceholder) || c.Spec.Password != "" {
			t.Fatalf("a planned spec must hold the placeholder and no password, got Password %q", c.Spec.Password)
		}
	}

	if err := r.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, vm := range client.created {
		if !hexPassword.MatchString(vm.Password) || vm.Username != "admin" {
			t.Fatalf("created %s with Username %q, Password %q; want admin and 48 random hex digits", vm.Name, vm.Username, vm.Password)
		}
		if seen[vm.Password] {
			t.Fatal("two VMs got the same password")
		}
		seen[vm.Password] = true
		if !strings.Contains(vm.StartupScript, "grove_guest_password='"+vm.Password+"'\n") {
			t.Errorf("%s: startup script doesn't set the password Orchard logs in with", vm.Name)
		}
		if strings.Contains(vm.StartupScript, "@GROVE_") {
			t.Errorf("%s: placeholder left in the created startup script", vm.Name)
		}
		// First, before Nomad can register and before the job user exists.
		i := strings.Index(vm.StartupScript, "grove:guest-password end")
		for _, later := range []string{"grove:nomad-tls begin", "grove:nomad-intro begin", MacOSJobUser} {
			if j := strings.Index(vm.StartupScript, later); j >= 0 && j < i {
				t.Errorf("%s: %q comes before the password swap", vm.Name, later)
			}
		}
	}

	// A VM holding its own password is up to date on the next tick.
	client.vms = nil
	for _, vm := range client.created {
		client.vms = append(client.vms, existingFrom(vm))
	}
	plan, err = r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("a VM with its own password must not read as drifted, got %+v", plan)
	}
}

func TestGuestPassword_OnlyWhereNeeded(t *testing.T) {
	custom := macosPool()
	custom.Password = "operator-chosen"
	for name, pool := range map[string]Pool{
		// Linux jobs run in containers the egress fence keeps off the VM's sshd.
		"linux pool":                 basePool(),
		"pool with its own password": custom,
	} {
		t.Run(name, func(t *testing.T) {
			// Neither needs the worker's fallback either.
			client := &fakeOrchardClient{workers: []orchard.Worker{legacyWorker("mac1")}}
			r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}
			plan, err := r.Plan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			vm := client.created[0]
			if strings.Contains(vm.StartupScript, "grove:guest-password") || vm.Password != pool.Password {
				t.Fatalf("want no password swap, got Password %q and script:\n%s", vm.Password, vm.StartupScript)
			}
		})
	}
}

func TestGuestPassword_RefusesWorkersThatCantRotate(t *testing.T) {
	pool := macosPool()
	name := VMName(pool.Name, "mac1", 0)
	// A VM made before the swap existed, still on admin/admin.
	old := (&Reconciler{}).vmSpec(pool, "mac1", name)
	old.StartupScript = guestPasswordBlockRE.ReplaceAllString(old.StartupScript, "")
	client := &fakeOrchardClient{
		workers: []orchard.Worker{legacyWorker("mac1"), onlineWorker("mac2")},
		vms:     []orchard.VM{existingFrom(old)},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blocked) != 1 || !strings.Contains(plan.Blocked[0].Reason, "worker mac1's Orchard") ||
		!strings.Contains(plan.Blocked[0].Reason, WorkerCapabilityGuestPasswordRotation) {
		t.Fatalf("want mac1 reported as unable to rotate, got %+v", plan.Blocked)
	}
	if len(plan.Creates) != 1 || plan.Creates[0].Worker != "mac2" {
		t.Fatalf("want a VM on mac2 only, got %+v", plan.Creates)
	}
	if len(plan.Deletes) != 1 || plan.Deletes[0].Name != name || !strings.HasPrefix(plan.Deletes[0].Reason, "unsafe: ") {
		t.Fatalf("mac1's admin/admin VM must be deleted as unsafe, got %+v", plan.Deletes)
	}
	if plan.BlockedError() == nil {
		t.Fatal("a refused worker must surface as a plan error")
	}
}

func TestGuestPassword_VMsWithoutTheSwapAreRecreated(t *testing.T) {
	pool := macosPool()
	name := VMName(pool.Name, "mac1", 0)
	before := (&Reconciler{}).vmSpec(pool, "mac1", name)
	before.StartupScript = guestPasswordBlockRE.ReplaceAllString(before.StartupScript, "")
	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms:     []orchard.VM{existingFrom(before)},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 1 || len(plan.Deletes) != 1 {
		t.Fatalf("a VM still on admin/admin must be recreated, got %+v", plan)
	}
}

func TestGuestPassword_SealedMacOSPoolsToo(t *testing.T) {
	pool := macosPool()
	pool.Network = NetworkIsolated
	script := buildStartupScript(pool, "mac1", "macos-mac1-0", Options{})
	if !strings.Contains(script, guestPasswordPlaceholder) {
		t.Fatalf("a sealed macOS VM's job can su to admin too:\n%s", script)
	}
}

// fakeDscl stands in for macOS's dscl over a one-account password file: -authonly checks it, and
// interactive mode (no arguments) runs "passwd /Users/<user> <old> <new>" lines from stdin. Like
// the real one for a SecureToken user (Tart's admin), it refuses a change that doesn't name the
// current password, even for root. Every invocation's argv is logged, so the test can tell the
// password never reached one.
const fakeDscl = `#!/bin/sh
echo "dscl $*" >> "$GROVE_TEST_DIR/argv"
db="$GROVE_TEST_DIR/password"
if [ "$#" -eq 4 ] && [ "$2" = -authonly ]; then
  [ "$(cat "$db")" = "$4" ]
  exit
fi
if [ "$#" -eq 1 ]; then
  while read -r cmd user old pw; do
    if [ "$cmd" = passwd ] && [ -n "$pw" ] && [ "$old" = "$(cat "$db")" ] && [ "$GROVE_TEST_DSCL_BROKEN" != 1 ]; then
      printf '%s' "$pw" > "$db"
    fi
  done
  exit 0
fi
exit 64
`

func runGuestPasswordBlock(t *testing.T, dir, current, password string, broken bool) error {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "dscl"), []byte(fakeDscl), 0o755); err != nil {
		t.Fatal(err)
	}
	// grove_priv falls back to sudo -n when not root; the fake is enough for either.
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "argv"))
	script := "set -eu\n" + strings.ReplaceAll(guestPasswordScript, guestPasswordPlaceholder, shQuote(password))
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GROVE_TEST_DIR="+dir)
	if broken {
		cmd.Env = append(cmd.Env, "GROVE_TEST_DSCL_BROKEN=1")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("script output:\n%s", out)
	}
	return err
}

func TestGuestPasswordScript_SwapsThePasswordOverStdin(t *testing.T) {
	dir := t.TempDir()
	const password = "0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := runGuestPasswordBlock(t, dir, imageGuestPassword, password, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "password")); string(got) != password {
		t.Fatalf("account password = %q, want the VM's own", got)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if strings.Contains(string(argv), password) {
		t.Fatalf("the password reached a command line:\n%s", argv)
	}

	// A rerun after a restart (Orchard now logs in with the new password) leaves it alone.
	if err := runGuestPasswordBlock(t, dir, password, password, false); err != nil {
		t.Fatal(err)
	}
	argv, _ = os.ReadFile(filepath.Join(dir, "argv"))
	if strings.Count(string(argv), "\n") != 1 {
		t.Fatalf("a rerun should only check the image password, got:\n%s", argv)
	}
}

func TestGuestPasswordScript_FailsWhileTheImagePasswordStillWorks(t *testing.T) {
	if err := runGuestPasswordBlock(t, t.TempDir(), imageGuestPassword, "new", true); err == nil {
		t.Fatal("the script must stop the VM from taking jobs while admin/admin still works")
	}
}
