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

func rotatingWorker(name string) orchard.Worker {
	w := onlineWorker(name)
	w.Capabilities = []string{"vm-endpoints", WorkerCapabilityGuestPasswordRotation}
	return w
}

var hexPassword = regexp.MustCompile(`^[0-9a-f]{48}$`)

func TestGuestPassword_EachVMGetsItsOwnAndOrchardTheSame(t *testing.T) {
	pool := macosPool()
	pool.PerWorker = 2
	client := &fakeOrchardClient{workers: []orchard.Worker{rotatingWorker("mac1")}}
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

func TestGuestPassword_OnlyWhereItIsSafeAndNeeded(t *testing.T) {
	custom := macosPool()
	custom.Password = "operator-chosen"
	for name, tc := range map[string]struct {
		pool   Pool
		worker orchard.Worker
	}{
		// The worker's Orchard would lock itself out of the shutdown script.
		"worker without the capability": {macosPool(), onlineWorker("mac1")},
		// Linux jobs run in containers the egress fence keeps off the VM's sshd.
		"linux pool": {basePool(), rotatingWorker("mac1")},
		"pool with its own password": {custom, rotatingWorker("mac1")},
	} {
		t.Run(name, func(t *testing.T) {
			client := &fakeOrchardClient{workers: []orchard.Worker{tc.worker}}
			r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{tc.pool}}}
			plan, err := r.Plan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Apply(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			vm := client.created[0]
			if strings.Contains(vm.StartupScript, "grove:guest-password") || vm.Password != tc.pool.Password {
				t.Fatalf("want no password swap, got Password %q and script:\n%s", vm.Password, vm.StartupScript)
			}
		})
	}
}

func TestGuestPassword_UpgradedWorkerRecreatesItsVMs(t *testing.T) {
	pool := macosPool()
	name := VMName(pool.Name, "mac1", 0)
	before := (&Reconciler{}).vmSpec(pool, "mac1", name)
	client := &fakeOrchardClient{
		workers: []orchard.Worker{rotatingWorker("mac1")},
		vms:     []orchard.VM{existingFrom(before)},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{pool}}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Creates) != 1 || len(plan.Deletes) != 1 {
		t.Fatalf("a VM still on admin/admin must be recreated once its worker can rotate, got %+v", plan)
	}
}

func TestGuestPassword_SealedMacOSPoolsToo(t *testing.T) {
	pool := macosPool()
	pool.Network = NetworkIsolated
	script := buildStartupScript(pool, "mac1", "macos-mac1-0", Options{rotateGuestPassword: true})
	if !strings.Contains(script, guestPasswordPlaceholder) {
		t.Fatalf("a sealed macOS VM's job can su to admin too:\n%s", script)
	}
}

// fakeDscl stands in for macOS's dscl over a one-account password file: -authonly checks it, and
// interactive mode (no arguments) runs "passwd /Users/<user> <password>" lines from stdin. Every
// invocation's argv is logged, so the test can tell the password never reached one.
const fakeDscl = `#!/bin/sh
echo "dscl $*" >> "$GROVE_TEST_DIR/argv"
db="$GROVE_TEST_DIR/password"
if [ "$#" -eq 4 ] && [ "$2" = -authonly ]; then
  [ "$(cat "$db")" = "$4" ]
  exit
fi
if [ "$#" -eq 1 ]; then
  while read -r cmd user pw; do
    if [ "$cmd" = passwd ] && [ "$GROVE_TEST_DSCL_BROKEN" != 1 ]; then
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
