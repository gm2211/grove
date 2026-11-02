package fleet

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/orchard"
)

func isolatedPool() Pool {
	pool := basePool()
	pool.Network = NetworkIsolated
	return pool
}

func isolatedMacOSPool() Pool {
	pool := isolatedPool()
	pool.Name = "macos"
	pool.Image = "ghcr.io/example/macos:latest"
	return pool
}

func isolatedOptions() Options {
	return Options{TailscaleAuthKey: "tskey-auth-abc", NomadRPCAddress: "100.64.0.10:4647"}
}

func TestSpecValidate_Network(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pool    Pool
		wantErr string
	}{
		{name: "unset", pool: Pool{Name: "linux", Image: "image", PerWorker: 1}},
		{name: "shared", pool: Pool{Name: "linux", Image: "image", PerWorker: 1, Network: "shared"}},
		{name: "isolated", pool: Pool{Name: "linux", Image: "image", PerWorker: 1, Network: "isolated"}},
		{
			name:    "unknown mode",
			pool:    Pool{Name: "linux", Image: "image", PerWorker: 1, Network: "softnet"},
			wantErr: `network must be "shared" or "isolated"`,
		},
		{name: "isolated macos pool", pool: Pool{Name: "macos", Image: "image", PerWorker: 1, Network: "isolated"}},
		{
			name:    "shared with docker socket",
			pool:    Pool{Name: "linux", Image: "image", PerWorker: 1, AllowDockerSocket: true},
			wantErr: "allowDockerSocket is no longer supported",
		},
		{
			name:    "isolated with docker socket",
			pool:    Pool{Name: "linux", Image: "image", PerWorker: 1, Network: "isolated", AllowDockerSocket: true},
			wantErr: "allowDockerSocket is no longer supported",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Spec{Pools: []Pool{tt.pool}}).Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_IsolatedNetwork(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: linux
    image: ghcr.io/example/linux:latest
    perWorker: 1
    network: isolated
`)
	spec, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !spec.Pools[0].Isolated() {
		t.Fatalf("pool should be isolated, got network %q", spec.Pools[0].Network)
	}
}

func TestPlan_IsolatedPool_CreatesSoftnetVMWithHostBlocked(t *testing.T) {
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{isolatedPool()}}, Options: isolatedOptions()}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Blocked) != 0 || plan.BlockedError() != nil {
		t.Fatalf("pool should not be blocked: %+v", plan.Blocked)
	}
	if len(plan.Creates) != 1 {
		t.Fatalf("want 1 create, got %+v", plan.Creates)
	}

	spec := plan.Creates[0].Spec
	if !spec.Softnet || !slices.Equal(spec.SoftnetBlock, []string{"out @host"}) {
		t.Fatalf("isolated VM must run on Softnet with the host blocked, got Softnet=%v SoftnetBlock=%q", spec.Softnet, spec.SoftnetBlock)
	}
	if !strings.Contains(spec.StartupScript, "# --- network: isolated (sealed) ---") {
		t.Fatalf("isolated VM is missing the sealed startup section:\n%s", spec.StartupScript)
	}
	// Nothing in a sealed VM may reach home, so nothing that would gets set up there, even with
	// a tailnet key and a Nomad address configured for the shared pools.
	for _, unwanted := range []string{"tskey-auth-abc", "tailscale up", "grove-meta.hcl", "100.64.0.10", "systemctl restart nomad"} {
		if strings.Contains(spec.StartupScript, unwanted) {
			t.Errorf("sealed VM startup script contains %q:\n%s", unwanted, spec.StartupScript)
		}
	}
	for _, unwanted := range []string{"nomad node drain", nodeTokenSecretPlaceholder} {
		if strings.Contains(spec.ShutdownScript, unwanted) {
			t.Errorf("sealed VM shutdown script contains %q:\n%s", unwanted, spec.ShutdownScript)
		}
	}
}

func TestPlan_SharedPool_NoSoftnet(t *testing.T) {
	for _, network := range []string{"", NetworkShared} {
		pool := basePool()
		pool.Network = network
		spec := (&Reconciler{Options: isolatedOptions()}).vmSpec(pool, "mac1", "linux-mac1-0")
		if spec.Softnet || len(spec.SoftnetBlock) != 0 {
			t.Fatalf("network %q: shared pool must keep Tart's default NAT, got Softnet=%v SoftnetBlock=%q", network, spec.Softnet, spec.SoftnetBlock)
		}
		if strings.Contains(spec.StartupScript, "network: isolated") {
			t.Fatalf("network %q: shared pool got the isolated startup section", network)
		}
	}

	// Writing `network: shared` explicitly must not recreate VMs built before the field existed.
	explicit := basePool()
	explicit.Network = NetworkShared
	if a, b := (&Reconciler{}).vmSpec(basePool(), "mac1", "linux-mac1-0"), (&Reconciler{}).vmSpec(explicit, "mac1", "linux-mac1-0"); a.StartupScript != b.StartupScript {
		t.Fatal(`network: shared changed the startup script relative to an unset network`)
	}
}

func TestPlan_IsolatedPool_NeedsNoTailnetConfig(t *testing.T) {
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{isolatedPool()}}}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Blocked) != 0 || len(plan.Creates) != 1 || !plan.Creates[0].Spec.Softnet {
		t.Fatalf("want one sealed VM with no tailnet key or Nomad address configured, got %+v", plan)
	}
}

func TestIsolationProblem_EveryPoolCanBeSealed(t *testing.T) {
	if problem := isolationProblem(isolatedPool()); problem != "" {
		t.Errorf("isolated Linux pool: %q", problem)
	}
	if problem := isolationProblem(isolatedMacOSPool()); problem != "" {
		t.Errorf("isolated macOS pool: %q", problem)
	}
}

func TestPlan_SwitchingPoolToIsolated_RecreatesVM(t *testing.T) {
	existingSpec := (&Reconciler{}).vmSpec(basePool(), "mac1", "linux-mac1-0")
	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1")},
		vms: []orchard.VM{{
			Name:            existingSpec.Name,
			Status:          "running",
			Image:           existingSpec.Image,
			CPU:             existingSpec.CPU,
			Memory:          existingSpec.Memory,
			RestartPolicy:   existingSpec.RestartPolicy,
			StartupScript:   existingSpec.StartupScript,
			ShutdownScript:  existingSpec.ShutdownScript,
			ShutdownTimeout: existingSpec.ShutdownTimeout,
			Labels:          map[string]string{LabelWorkerPin: "mac1"},
		}},
	}
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{isolatedPool()}}, Options: isolatedOptions()}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Deletes) != 1 || plan.Deletes[0].Reason != "spec changed" {
		t.Fatalf("want the shared VM deleted as changed, got %+v", plan.Deletes)
	}
	if len(plan.Creates) != 1 || !plan.Creates[0].Spec.Softnet {
		t.Fatalf("want an isolated replacement, got %+v", plan.Creates)
	}
}

func TestSpecDrifted_Softnet(t *testing.T) {
	desired := (&Reconciler{Options: isolatedOptions()}).vmSpec(isolatedPool(), "mac1", "linux-mac1-0")
	observed := orchard.VM{
		Status:          "running",
		Image:           desired.Image,
		CPU:             desired.CPU,
		Memory:          desired.Memory,
		RestartPolicy:   desired.RestartPolicy,
		StartupScript:   desired.StartupScript,
		ShutdownScript:  desired.ShutdownScript,
		ShutdownTimeout: desired.ShutdownTimeout,
		Softnet:         true,
		SoftnetBlock:    []string{"out @host"},
	}
	if specDrifted(desired, observed) {
		t.Fatal("a VM matching the isolated spec should not drift")
	}

	noSoftnet := observed
	noSoftnet.Softnet = false
	if !specDrifted(desired, noSoftnet) {
		t.Fatal("losing Softnet must count as drift")
	}

	hostAllowed := observed
	hostAllowed.SoftnetBlock = nil
	if !specDrifted(desired, hostAllowed) {
		t.Fatal("losing the host block must count as drift")
	}
}

func TestBuildStartupScript_Isolated(t *testing.T) {
	script := buildStartupScript(isolatedPool(), "mac1", "linux-mac1-0", isolatedOptions())

	// Nomad and Tailscale go off first, then DNS, then the fence, and only then Docker, so no job
	// container ever runs unfenced.
	last := -1
	for _, want := range []string{
		"grove_vm='linux-mac1-0'",
		"for grove_unit in nomad.service tailscaled.service; do\n  grove_priv systemctl disable --now \"$grove_unit\"",
		"grove_priv rm -f /run/grove-draining",
		"grove_priv rm -f /etc/resolv.conf",
		"nameserver 1.1.1.1\nnameserver 8.8.8.8\nGROVE_RESOLV_CONF",
		"grove_priv tee /usr/local/sbin/grove-egress-fence",
		"grove_priv tee /etc/systemd/system/docker.service.d/grove-egress-fence.conf",
		"grove_priv /usr/local/sbin/grove-egress-fence",
		"grove_priv systemctl enable --now docker.service",
	} {
		at := strings.Index(script, want)
		if at < 0 {
			t.Fatalf("sealed startup script missing %q:\n%s", want, script)
		}
		if at < last {
			t.Fatalf("sealed startup script has %q out of order:\n%s", want, script)
		}
		last = at
	}

	for _, unwanted := range []string{"tailscale up", "--authkey", "--auth-key", "grove-rpc.hcl", "grove_meta"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("sealed startup script must not contain %q:\n%s", unwanted, script)
		}
	}

	pool := isolatedPool()
	pool.StartupScript = "echo custom"
	if script := buildStartupScript(pool, "mac1", "linux-mac1-0", Options{}); !strings.HasSuffix(script, "# --- pool.startupScript ---\necho custom\n") {
		t.Errorf("the pool's own startup script must run last:\n%s", script)
	}
}

// TestBuildStartupScript_IsolatedRunsThroughFakeSudo executes the isolated startup script as an
// unprivileged guest user against stub commands and checks every privileged step it takes, in
// order, and the files it writes.
func TestBuildStartupScript_IsolatedRunsThroughFakeSudo(t *testing.T) {
	fakeBin := newFakeBin(t)
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "sudo.log")
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	writeExecutable("id", "#!/bin/sh\necho 502\n")
	writeExecutable("uname", "#!/bin/sh\necho Linux\n")
	writeExecutable("systemctl", "#!/bin/sh\nexit 64\n") // only ever run through sudo
	writeExecutable("sudo", `#!/bin/sh
set -eu
[ "${1:-}" = "-n" ] || exit 65
shift
printf '%s\n' "$*" >> "$GROVE_TEST_SUDO_LOG"
if [ "$1" = "tee" ]; then
  shift
  if [ "$1" = "-a" ]; then
    shift
    mkdir -p "$(dirname "$GROVE_TEST_ROOT$1")"
    cat >> "$GROVE_TEST_ROOT$1"
  else
    mkdir -p "$(dirname "$GROVE_TEST_ROOT$1")"
    cat > "$GROVE_TEST_ROOT$1"
  fi
fi
`)

	script := buildStartupScript(isolatedPool(), "mac1", "linux-mac1-0", isolatedOptions())
	cmd := guestCmd(t, fakeBin, "-c", script)
	cmd.Env = append(cmd.Env,
		"GROVE_TEST_SUDO_LOG="+logPath,
		"GROVE_TEST_ROOT="+root,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sealed startup script failed through fake sudo: %v\n%s", err, output)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read sudo log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(log)), "\n")
	want := []string{
		"systemctl disable --now nomad.service",
		"systemctl disable --now tailscaled.service",
		"rm -f /run/grove-draining",
		"rm -f /etc/resolv.conf",
		"tee /etc/resolv.conf",
		"mkdir -p /usr/local/sbin /etc/systemd/system/docker.service.d",
		"tee /usr/local/sbin/grove-egress-fence",
		"chmod 0755 /usr/local/sbin/grove-egress-fence",
		"tee /etc/systemd/system/docker.service.d/grove-egress-fence.conf",
		"systemctl daemon-reload",
		"/usr/local/sbin/grove-egress-fence",
		"systemctl enable --now docker.service",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("privileged steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for path, want := range map[string]string{
		"/etc/resolv.conf":                                             "nameserver 1.1.1.1\nnameserver 8.8.8.8\n",
		"/usr/local/sbin/grove-egress-fence":                           egressFenceScript,
		"/etc/systemd/system/docker.service.d/grove-egress-fence.conf": egressFenceDockerDropIn,
	} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(data) != want {
			t.Errorf("%s =\n%s\nwant:\n%s", path, data, want)
		}
	}
}

// TestEgressFenceScript_IdempotentAndComplete runs the fence twice against a stub iptables that
// keeps chains as files, before Docker has created DOCKER-USER and after, and checks the result
// is the same complete rule set with each jump present exactly once.
func TestEgressFenceScript_IdempotentAndComplete(t *testing.T) {
	for _, dockerStarted := range []bool{false, true} {
		fakeBin := newFakeBin(t)
		chains := t.TempDir()
		if err := os.WriteFile(filepath.Join(fakeBin, "iptables"), []byte(`#!/bin/sh
set -eu
[ "$1" = "-w" ] || exit 65
shift
op=$1
chain=$2
shift 2
f="$GROVE_TEST_CHAINS/$chain"
case "$op" in
  -N) [ ! -e "$f" ] || exit 1; : > "$f" ;;
  -F) [ -e "$f" ] || exit 1; : > "$f" ;;
  -A) [ -e "$f" ] || exit 1; printf '%s\n' "$*" >> "$f" ;;
  -I) [ -e "$f" ] || exit 1; [ "$1" = 1 ] || exit 66; shift
      { printf '%s\n' "$*"; cat "$f"; } > "$f.new"; mv "$f.new" "$f" ;;
  -C) [ -e "$f" ] || exit 1; grep -qxF -- "$*" "$f" ;;
  *) exit 67 ;;
esac
`), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chains, "INPUT"), []byte("-p tcp --dport 22 -j ACCEPT\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if dockerStarted {
			if err := os.WriteFile(filepath.Join(chains, "DOCKER-USER"), []byte("-j RETURN\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		for run := 0; run < 2; run++ {
			cmd := guestCmd(t, fakeBin, "-c", egressFenceScript)
			cmd.Env = append(cmd.Env, "GROVE_TEST_CHAINS="+chains)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("dockerStarted=%v run %d: fence failed: %v\n%s", dockerStarted, run, err, output)
			}
		}

		readChain := func(name string) string {
			data, err := os.ReadFile(filepath.Join(chains, name))
			if err != nil {
				t.Fatalf("read chain %s: %v", name, err)
			}
			return string(data)
		}
		reject := " -j REJECT --reject-with icmp-admin-prohibited\n"
		if got, want := readChain("GROVE-EGRESS"), "-o docker0 -j RETURN\n-o br-+ -j RETURN\n"+
			"-d 10.0.0.0/8"+reject+"-d 100.64.0.0/10"+reject+"-d 169.254.0.0/16"+reject+
			"-d 172.16.0.0/12"+reject+"-d 192.168.0.0/16"+reject; got != want {
			t.Errorf("dockerStarted=%v GROVE-EGRESS =\n%s\nwant:\n%s", dockerStarted, got, want)
		}
		if got, want := readChain("GROVE-INGRESS"), "-m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n"+
			"-i docker0 -j DROP\n-i br-+ -j DROP\n"; got != want {
			t.Errorf("dockerStarted=%v GROVE-INGRESS =\n%s\nwant:\n%s", dockerStarted, got, want)
		}

		dockerUser := "-j GROVE-EGRESS\n"
		if dockerStarted {
			dockerUser += "-j RETURN\n"
		}
		if got := readChain("DOCKER-USER"); got != dockerUser {
			t.Errorf("dockerStarted=%v DOCKER-USER =\n%s\nwant:\n%s", dockerStarted, got, dockerUser)
		}
		if got, want := readChain("INPUT"), "-j GROVE-INGRESS\n-p tcp --dport 22 -j ACCEPT\n"; got != want {
			t.Errorf("dockerStarted=%v INPUT =\n%s\nwant:\n%s", dockerStarted, got, want)
		}
	}
}

// TestBuildShutdownScript_SealedWaitsForJobs runs a sealed VM's shutdown script against a stub
// docker that reports a job for two polls: it must mark the VM draining first, then wait for the
// job to finish, and never touch Nomad.
func TestBuildShutdownScript_SealedWaitsForJobs(t *testing.T) {
	fakeBin := newFakeBin(t)
	logPath := filepath.Join(t.TempDir(), "sudo.log")
	counter := filepath.Join(t.TempDir(), "polls")
	for name, body := range map[string]string{
		"id":     "#!/bin/sh\necho 502\n",
		"sleep":  "#!/bin/sh\nexit 0\n",
		"nomad":  "#!/bin/sh\nexit 70\n",
		"docker": "#!/bin/sh\nn=$(cat \"$GROVE_TEST_COUNTER\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > \"$GROVE_TEST_COUNTER\"\n[ $n -gt 2 ] || echo abc123\n",
		"sudo":   "#!/bin/sh\n[ \"$1\" = -n ] || exit 65\nshift\nprintf '%s\\n' \"$*\" >> \"$GROVE_TEST_SUDO_LOG\"\nexec \"$@\"\n",
		"touch":  "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	script, timeout := buildShutdownScript(isolatedPool(), true)
	if timeout != defaultShutdownTimeout {
		t.Errorf("timeout = %s", timeout)
	}
	cmd := guestCmd(t, fakeBin, "-c", script)
	cmd.Env = append(cmd.Env, "GROVE_TEST_SUDO_LOG="+logPath, "GROVE_TEST_COUNTER="+counter)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sealed shutdown script failed: %v\n%s", err, output)
	}
	log, _ := os.ReadFile(logPath)
	want := "touch /run/grove-draining\n" + strings.Repeat("docker ps --quiet --filter label=grove.alloc\n", 3)
	if string(log) != want {
		t.Fatalf("privileged steps:\n%s\nwant:\n%s", log, want)
	}
	if strings.Contains(script, "nomad") || strings.Contains(script, nodeTokenSecretPlaceholder) {
		t.Errorf("sealed shutdown script mentions Nomad:\n%s", script)
	}
}

func TestPlan_IsolatedMacOSPool_CreatesSealedVM(t *testing.T) {
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
	opts := isolatedOptions()
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{isolatedMacOSPool()}}, Options: opts}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Blocked) != 0 || len(plan.Creates) != 1 {
		t.Fatalf("want one sealed macOS VM, got %+v", plan)
	}
	spec := plan.Creates[0].Spec
	if !spec.Softnet || !slices.Equal(spec.SoftnetBlock, []string{"out @host"}) {
		t.Fatalf("sealed macOS VM must run on Softnet with the host blocked, got Softnet=%v SoftnetBlock=%q", spec.Softnet, spec.SoftnetBlock)
	}
	if !strings.Contains(spec.StartupScript, "# --- network: isolated (sealed, macOS) ---") {
		t.Fatalf("missing the sealed macOS section:\n%s", spec.StartupScript)
	}
	// No secret and nothing that reaches home, even with a tailnet key and a Nomad address
	// configured for the shared pools.
	for _, unwanted := range []string{"tskey-auth-abc", "tailscale up", "--authkey", "grove-meta.hcl", "grove-rpc.hcl",
		"100.64.0.10", "launchctl kickstart", "launchctl bootstrap", "systemctl", "docker", "egress-fence"} {
		if strings.Contains(spec.StartupScript, unwanted) {
			t.Errorf("sealed macOS startup script contains %q:\n%s", unwanted, spec.StartupScript)
		}
	}
	for _, unwanted := range []string{"nomad", "NOMAD_TOKEN", nodeTokenSecretPlaceholder, nodeTokenAccessorPlaceholder, "docker", "tskey"} {
		if strings.Contains(spec.ShutdownScript, unwanted) {
			t.Errorf("sealed macOS shutdown script contains %q:\n%s", unwanted, spec.ShutdownScript)
		}
	}

	// With Nomad ACLs on, a shared VM's scripts carry node-token blocks; a sealed one never does.
	r.Options.NodeTokens = &fakeNodeTokens{}
	plan, err = r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan with node tokens: %v", err)
	}
	spec = plan.Creates[0].Spec
	if strings.Contains(spec.StartupScript+spec.ShutdownScript, "grove:node-token") || strings.Contains(spec.StartupScript, "grove:client-acl") {
		t.Errorf("sealed macOS VM got a Nomad token block:\n%s\n%s", spec.StartupScript, spec.ShutdownScript)
	}
}

// TestPlan_SealedMacOSVMDeletedAfterItsJob_IsRecreated: the job runner deletes a sealed macOS VM
// once its one job is over; the next reconcile simply creates it again under the same name, from
// the image, with the same sealed spec.
func TestPlan_SealedMacOSVMDeletedAfterItsJob_IsRecreated(t *testing.T) {
	pool := isolatedMacOSPool()
	r := &Reconciler{Spec: &Spec{Pools: []Pool{pool}}}
	want := r.vmSpec(pool, "mac1", "macos-mac1-0")
	running := orchard.VM{
		Name: want.Name, Status: "running", Image: want.Image, CPU: want.CPU, Memory: want.Memory,
		RestartPolicy: want.RestartPolicy, StartupScript: want.StartupScript, ShutdownScript: want.ShutdownScript,
		ShutdownTimeout: want.ShutdownTimeout, Softnet: true, SoftnetBlock: []string{"out @host"},
		Labels: map[string]string{LabelWorkerPin: "mac1"},
	}
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}, vms: []orchard.VM{running}}
	r.Client = client

	plan, err := r.Plan(context.Background())
	if err != nil || !plan.Empty() {
		t.Fatalf("Plan with the VM running = %+v, %v; want nothing to do", plan, err)
	}

	client.vms = nil // the runner deleted it
	plan, err = r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Deletes) != 0 || len(plan.Creates) != 1 || plan.Creates[0].Spec.Name != "macos-mac1-0" {
		t.Fatalf("plan = %+v, want macos-mac1-0 recreated", plan)
	}
	if got := plan.Creates[0].Spec; got.StartupScript != want.StartupScript || !got.Softnet {
		t.Fatalf("recreated spec differs from the original sealed spec: %+v", got)
	}
}

func TestBuildStartupScript_IsolatedMacOS(t *testing.T) {
	script := buildStartupScript(isolatedMacOSPool(), "mac1", "macos-mac1-0", isolatedOptions())

	// Nomad and Tailscale go off first; then DNS, then the job user.
	last := -1
	for _, want := range []string{
		"grove_vm='macos-mac1-0'",
		"for grove_service in com.grove.nomad homebrew.mxcl.tailscale; do",
		`grove_priv launchctl disable "system/$grove_service"`,
		`grove_priv launchctl bootout "system/$grove_service"`,
		"grove_priv rm -f /private/var/run/grove-draining",
		`grove_priv networksetup -setdnsservers "$grove_dns_service" 1.1.1.1 8.8.8.8`,
		macosJobUserScript,
	} {
		at := strings.Index(script, want)
		if at < 0 {
			t.Fatalf("sealed macOS startup script missing %q:\n%s", want, script)
		}
		if at < last {
			t.Fatalf("sealed macOS startup script has %q out of order:\n%s", want, script)
		}
		last = at
	}
	if strings.Count(script, "dscl . -create /Users/"+MacOSJobUser+" UniqueID") != 1 {
		t.Errorf("the job user must be created by the shared macosJobUserScript, once:\n%s", script)
	}

	pool := isolatedMacOSPool()
	pool.HostXcode = &HostXcodeConfig{Path: "/Applications/Xcode.app", BuildVersion: "27A266a"}
	pool.StartupScript = "echo custom"
	script = buildStartupScript(pool, "mac1", "macos-mac1-0", Options{})
	if !strings.Contains(script, "xcrun --sdk macosx --show-sdk-path") || strings.Contains(script, "launchctl bootstrap") {
		t.Errorf("a sealed pool with hostXcode checks Xcode but never brings Nomad back:\n%s", script)
	}
	if !strings.HasSuffix(script, "# --- pool.startupScript ---\necho custom\n") {
		t.Errorf("the pool's own startup script must run last:\n%s", script)
	}
}

// TestBuildStartupScript_IsolatedMacOSRunsThroughFakeSudo executes the sealed macOS startup script
// as an unprivileged guest user against stub commands and checks every privileged step, in order.
func TestBuildStartupScript_IsolatedMacOSRunsThroughFakeSudo(t *testing.T) {
	fakeBin := newFakeBin(t)
	logPath := filepath.Join(t.TempDir(), "sudo.log")
	for name, body := range map[string]string{
		"id":    "#!/bin/sh\necho 502\n",
		"uname": "#!/bin/sh\necho Darwin\n",
		// Only -listallnetworkservices runs unprivileged.
		"networksetup": "#!/bin/sh\n[ \"$1\" = -listallnetworkservices ] || exit 64\n" +
			"printf '%s\\n' 'An asterisk (*) denotes that a network service is disabled.' 'Ethernet' '*Thunderbolt Bridge' 'Wi-Fi'\n",
		"dscl":      "#!/bin/sh\n[ \"$2\" = -read ] || exit 64\nexit 56\n", // no job user yet
		"launchctl": "#!/bin/sh\nexit 64\n",
		"sudo":      "#!/bin/sh\nset -eu\n[ \"${1:-}\" = \"-n\" ] || exit 65\nshift\nprintf '%s\\n' \"$*\" >> \"$GROVE_TEST_SUDO_LOG\"\n",
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	script, _, _ := fakeMacOSLockdown(t, fakeBin, buildStartupScript(isolatedMacOSPool(), "mac1", "macos-mac1-0", isolatedOptions()), true)
	cmd := guestCmd(t, fakeBin, "-c", script)
	cmd.Env = append(cmd.Env, "GROVE_TEST_SUDO_LOG="+logPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sealed macOS startup script failed through fake sudo: %v\n%s", err, output)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read sudo log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(log)), "\n")
	want := []string{
		"launchctl disable system/com.grove.nomad",
		"launchctl bootout system/com.grove.nomad",
		"launchctl disable system/homebrew.mxcl.tailscale",
		"launchctl bootout system/homebrew.mxcl.tailscale",
		"pkill -x nomad",
		"pkill -x tailscaled",
		"rm -f /private/var/run/grove-draining",
		"networksetup -setdnsservers Ethernet 1.1.1.1 8.8.8.8",
		"networksetup -setdnsservers Thunderbolt Bridge 1.1.1.1 8.8.8.8",
		"networksetup -setdnsservers Wi-Fi 1.1.1.1 8.8.8.8",
		"dscacheutil -flushcache",
		"killall -HUP mDNSResponder",
		"dscl . -create /Groups/_grovejob",
	}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("privileged steps:\n%s\nwant them to start with:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	tail := strings.Join(got[len(want):], "\n")
	for _, step := range []string{"dscl . -create /Users/_grovejob UniqueID 7460", "mkdir -p /private/var/grove-job", "chmod 0700 /private/var/grove-job",
		"security authorizationdb write system.privilege.admin deny", "tee /usr/local/etc/grove/pf-jobs.conf", "tee /Library/LaunchDaemons/com.grove.pf-jobs.plist"} {
		if !strings.Contains(tail, step) {
			t.Errorf("privileged steps miss %q:\n%s", step, tail)
		}
	}
}

// TestBuildShutdownScript_SealedMacOSWaitsForJobs runs a sealed macOS VM's shutdown script against
// a stub pgrep that finds job processes for two polls: it must mark the VM draining first, then
// wait for the job user's processes to end, and never touch Nomad or docker.
func TestBuildShutdownScript_SealedMacOSWaitsForJobs(t *testing.T) {
	fakeBin := newFakeBin(t)
	logPath := filepath.Join(t.TempDir(), "steps.log")
	counter := filepath.Join(t.TempDir(), "polls")
	for name, body := range map[string]string{
		"id":     "#!/bin/sh\necho 502\n",
		"sleep":  "#!/bin/sh\nexit 0\n",
		"nomad":  "#!/bin/sh\nexit 70\n",
		"docker": "#!/bin/sh\nexit 70\n",
		"pgrep": "#!/bin/sh\nprintf 'pgrep %s\\n' \"$*\" >> \"$GROVE_TEST_LOG\"\n" +
			"n=$(cat \"$GROVE_TEST_COUNTER\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > \"$GROVE_TEST_COUNTER\"\n[ $n -le 2 ]\n",
		"sudo":  "#!/bin/sh\n[ \"$1\" = -n ] || exit 65\nshift\nprintf 'sudo %s\\n' \"$*\" >> \"$GROVE_TEST_LOG\"\nexec \"$@\"\n",
		"touch": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	script, timeout := buildShutdownScript(isolatedMacOSPool(), true)
	if timeout != defaultShutdownTimeout {
		t.Errorf("timeout = %s", timeout)
	}
	if !strings.Contains(script, "grove_deadline=$(( $(date +%s) + 9000 ))") {
		t.Errorf("shutdown wait must be bounded by grove_deadline:\n%s", script)
	}
	cmd := guestCmd(t, fakeBin, "-c", script)
	cmd.Env = append(cmd.Env, "GROVE_TEST_LOG="+logPath, "GROVE_TEST_COUNTER="+counter)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sealed macOS shutdown script failed: %v\n%s", err, output)
	}
	log, _ := os.ReadFile(logPath)
	want := "sudo touch /private/var/run/grove-draining\n" + strings.Repeat("pgrep -U _grovejob\n", 3)
	if string(log) != want {
		t.Fatalf("steps:\n%s\nwant:\n%s", log, want)
	}
	if strings.Contains(script, "nomad") || strings.Contains(script, nodeTokenSecretPlaceholder) {
		t.Errorf("sealed macOS shutdown script mentions Nomad:\n%s", script)
	}
}
