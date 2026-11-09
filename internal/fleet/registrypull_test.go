package fleet

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/registryauth"
)

const (
	registryTestToken = "ghp_registrySecretToken0123456789"
	registryTestImage = "ghcr.io/gm2211/argos-ci"
)

// registryFixture is an enrolment fixture whose pool runs a GHCR image and whose reconciler reads
// a real registryauth.Store.
type registryFixture struct {
	*enrolFixture
	store *registryauth.Store
	// stdins is what each exec read on stdin, which is where the secret travels.
	stdins []string
}

func newRegistryFixture(t *testing.T, runnerImage string) *registryFixture {
	t.Helper()
	f := newEnrolFixture(t)
	store, err := registryauth.NewStore(filepath.Join(t.TempDir(), "registry-pull.json"))
	if err != nil {
		t.Fatal(err)
	}
	rf := &registryFixture{enrolFixture: f, store: store}
	f.r.Spec.Pools[0].RunnerImage = runnerImage
	f.r.Options.RegistryPull = store
	inner := f.client.exec
	f.client.exec = func(vm string, command []string, stdin string) (int, string) {
		rf.stdins = append(rf.stdins, stdin)
		return inner(vm, command, stdin)
	}
	return rf
}

func (f *registryFixture) arm(token string, repos ...string) {
	f.t.Helper()
	if _, err := f.store.Enable(registryauth.EnableOptions{User: "gm2211", Token: token, Repos: repos}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *registryFixture) guestConfig() (string, os.FileMode) {
	f.t.Helper()
	path := filepath.Join(f.guest.registry, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatalf("registry login missing on the guest: %v", err)
	}
	info, _ := os.Stat(path)
	return string(data), info.Mode().Perm()
}

func TestEnrol_RegistryLoginIsWrittenRootOnlyWhenTheCredentialCoversThePoolsImage(t *testing.T) {
	f := newRegistryFixture(t, registryTestImage+":v1")
	f.arm(registryTestToken, "gm2211/argos-ci")
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}

	got, mode := f.guestConfig()
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(got), &cfg); err != nil {
		t.Fatalf("config.json is not JSON: %v\n%s", err, got)
	}
	decoded, _ := base64.StdEncoding.DecodeString(cfg.Auths["ghcr.io"].Auth)
	if string(decoded) != "gm2211:"+registryTestToken || len(cfg.Auths) != 1 {
		t.Fatalf("config.json auths = %+v; want one ghcr.io login for gm2211", cfg.Auths)
	}
	if mode != 0o600 {
		t.Errorf("config.json mode = %v, want 0600", mode)
	}
	if info, err := os.Stat(f.guest.registry); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("registry dir = %v, %v; want mode 0700", info, err)
	}
	entries, _ := os.ReadDir(f.guest.registry)
	if len(entries) != 1 {
		t.Errorf("registry dir holds %v, want config.json only", entries)
	}

	// The secret rides stdin and nothing else: not argv (sudo logs it), not the VM's spec, not
	// what Enrol remembers or reports, not the startup script.
	if len(f.stdins) != 1 || !strings.Contains(f.stdins[0], base64.StdEncoding.EncodeToString([]byte(got))) {
		t.Fatalf("the login must travel in the exec's stdin")
	}
	for _, exec := range f.execs {
		for _, leak := range []string{registryTestToken, "gm2211:", cfg.Auths["ghcr.io"].Auth} {
			if strings.Contains(exec, leak) {
				t.Errorf("exec argv holds the registry secret %q: %s", leak, exec)
			}
		}
	}
	for _, vm := range f.client.vms {
		if strings.Contains(vm.StartupScript, registryTestToken) {
			t.Error("a VM's startup script holds the registry token")
		}
	}
	planned := (&Reconciler{Options: f.r.Options}).vmSpec(f.r.Spec.Pools[0], "mac1", "linux-mac1-0")
	if strings.Contains(planned.StartupScript, registryTestToken) {
		t.Error("a planned spec holds the registry token")
	}
	for _, text := range append(f.state.Enrolled, mapValues(f.state.Refused)...) {
		if strings.Contains(text, registryTestToken) {
			t.Errorf("enrol state reports the token: %s", text)
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestEnrol_RegistryLoginIsEmptyWhenNoCredentialCoversThePool(t *testing.T) {
	for name, setup := range map[string]func(*registryFixture){
		"disarmed":                          func(*registryFixture) {},
		"credential for another repository": func(f *registryFixture) { f.arm(registryTestToken, "gm2211/other") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRegistryFixture(t, registryTestImage+":v1")
			setup(f)
			if err := f.enrol(); err != nil {
				t.Fatal(err)
			}
			got, mode := f.guestConfig()
			if got != registryauth.EmptyDockerConfig || mode != 0o600 {
				t.Fatalf("config.json = %q (%v), want an empty-auths file at 0600", got, mode)
			}
			if strings.Contains(strings.Join(f.stdins, "\n"), base64.StdEncoding.EncodeToString([]byte(registryTestToken))) {
				t.Error("the empty login still carries a token")
			}
		})
	}
}

func TestEnrol_RegistryLoginCoversEnvironmentsAllow(t *testing.T) {
	f := newRegistryFixture(t, "ghcr.io/gm2211/grove-runner:latest")
	f.r.Spec.Pools[0].Environments = &PoolEnvironments{Allow: []string{"ghcr.io/gm2211/argos-ci"}}
	f.arm(registryTestToken, "gm2211/argos-ci")
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.guestConfig(); !strings.Contains(got, "ghcr.io") {
		t.Fatalf("config.json = %q; the environments.allow repository is in scope, so the login belongs there", got)
	}
}

func TestEnrol_RegistryLoginFollowsArmRotateDisarmOnTheNextTick(t *testing.T) {
	f := newRegistryFixture(t, registryTestImage+":v1")
	f.register("ready")
	if err := f.enrol(); err != nil { // disarmed: an empty login
		t.Fatal(err)
	}
	if err := f.enrol(); err != nil || len(f.execs) != 1 {
		t.Fatalf("an unchanged login must not be pushed again (%d execs, %v)", len(f.execs), err)
	}

	f.arm(registryTestToken, "gm2211/argos-ci")
	if err := f.enrol(); err != nil || len(f.execs) != 2 {
		t.Fatalf("arming must push the login on the next tick (%d execs, %v)", len(f.execs), err)
	}
	if got, _ := f.guestConfig(); !strings.Contains(got, "ghcr.io") {
		t.Fatalf("armed login not on the guest: %q", got)
	}
	if err := f.enrol(); err != nil || len(f.execs) != 2 {
		t.Fatalf("an unchanged login must not be pushed again (%d execs, %v)", len(f.execs), err)
	}

	f.arm(registryTestToken+"rotated", "gm2211/argos-ci")
	if err := f.enrol(); err != nil || len(f.execs) != 3 {
		t.Fatalf("rotating must push the new login (%d execs, %v)", len(f.execs), err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("gm2211:" + registryTestToken + "rotated"))
	if got, _ := f.guestConfig(); !strings.Contains(got, want) {
		t.Fatalf("rotated login not on the guest: %q", got)
	}

	if _, err := f.store.Disable(); err != nil {
		t.Fatal(err)
	}
	if err := f.enrol(); err != nil || len(f.execs) != 4 {
		t.Fatalf("disarming must push an empty login on the next tick (%d execs, %v)", len(f.execs), err)
	}
	if got, _ := f.guestConfig(); got != registryauth.EmptyDockerConfig {
		t.Fatalf("disarmed login = %q, want empty auths", got)
	}
}

func TestEnrol_NoRegistryStoreLeavesTheGuestAlone(t *testing.T) {
	f := newEnrolFixture(t)
	if err := f.enrol(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.guest.registry, "config.json")); err == nil {
		t.Fatal("with no registry store the enrolment wrote a login")
	}
}

// The startup script's docker block, run the way a Linux VM runs it.
func TestLinuxDockerAuthScript_WritesTheCompletePluginBlockAndAnEmptyLogin(t *testing.T) {
	g := newGuestDirs(t)
	prelude := "set -eu\ngrove_priv() { \"$@\"; }\nmkdir -p " + shQuote(g.nomad) + "\n"
	run := func() {
		t.Helper()
		if out, err := guestCmd(t, "", "-c", prelude+g.rewrite(linuxDockerAuthScript)).CombinedOutput(); err != nil {
			t.Fatalf("docker auth block failed: %v\n%s", err, out)
		}
	}
	run()
	login := filepath.Join(g.registry, "config.json")
	if data, err := os.ReadFile(login); err != nil || string(data) != registryauth.EmptyDockerConfig+"\n" {
		t.Fatalf("login = %q, %v; want an empty-auths file the driver can read", data, err)
	}
	if info, _ := os.Stat(login); info.Mode().Perm() != 0o600 {
		t.Errorf("login mode = %v, want 0600", info.Mode().Perm())
	}
	if info, _ := os.Stat(g.registry); info.Mode().Perm() != 0o700 {
		t.Errorf("login dir mode = %v, want 0700", info.Mode().Perm())
	}
	if got := g.read(t, "grove-docker.hcl"); got != g.rewrite(linuxDockerPluginBlock) {
		t.Fatalf("grove-docker.hcl = %q", got)
	}

	// A login Enrol already wrote survives a (re)run of the startup script.
	if err := os.WriteFile(login, []byte(`{"auths":{"ghcr.io":{"auth":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	if data, _ := os.ReadFile(login); !strings.Contains(string(data), `"ghcr.io"`) {
		t.Fatalf("the startup script clobbered the enrolled login: %q", data)
	}
}

// Nomad merges plugin blocks of the same name by REPLACING the config map (see
// linuxDockerAuthScript), so grove-docker.hcl must restate every setting of client.hcl's block.
func TestLinuxDockerPluginBlockMatchesTheImagesClientHCL(t *testing.T) {
	data, err := os.ReadFile("../../images/linux-worker/files/client.hcl")
	if err != nil {
		t.Fatal(err)
	}
	var block []string
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `plugin "docker"`) {
			in = true
		}
		if in && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			block = append(block, trimmed)
		}
		if in && trimmed == "}" && len(block) > 1 && strings.Count(strings.Join(block, "\n"), "{") == strings.Count(strings.Join(block, "\n"), "}") {
			break
		}
	}
	var want []string
	for _, line := range strings.Split(linuxDockerPluginBlock, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			want = append(want, trimmed)
		}
	}
	if strings.Join(block, "\n") != strings.Join(want, "\n") {
		t.Fatalf("images/linux-worker/files/client.hcl's docker plugin block drifted from linuxDockerPluginBlock:\n--- client.hcl ---\n%s\n--- fleet ---\n%s", strings.Join(block, "\n"), strings.Join(want, "\n"))
	}
}

func TestSpecWarnsOnARunnerImageWithNoRegistryHost(t *testing.T) {
	for image, warns := range map[string]bool{
		"grove-runner:studio-v1":             true,
		"acme/grove-runner:v1":               true,
		"ghcr.io/gm2211/grove-runner:v1":     false,
		"localhost/grove-runner:v1":          false,
		"registry.example:5000/grove-runner": false,
		"":                                   false,
	} {
		spec := &Spec{Pools: []Pool{{Name: "linux", RunnerImage: image}}}
		if got := len(spec.Warnings()) > 0; got != warns {
			t.Errorf("runnerImage %q: warns = %v, want %v (%v)", image, got, warns, spec.Warnings())
		}
	}
}
