package fleet

import (
	"context"
	"os"
	"os/exec"
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
		{
			name:    "isolated macos pool",
			pool:    Pool{Name: "macos", Image: "image", PerWorker: 1, Network: "isolated"},
			wantErr: "only supported for Linux pools",
		},
		{
			name:    "isolated with docker socket",
			pool:    Pool{Name: "linux", Image: "image", PerWorker: 1, Network: "isolated", AllowDockerSocket: true},
			wantErr: "allowDockerSocket can't be combined",
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
	if !strings.Contains(spec.StartupScript, "# --- network: isolated ---") {
		t.Fatalf("isolated VM is missing the isolated startup section:\n%s", spec.StartupScript)
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

func TestPlan_IsolatedPool_BlockedWithoutUsableConfig(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{name: "no auth key", mutate: func(o *Options) { o.TailscaleAuthKey = "" }, wantErr: "tailscale.authKey"},
		{name: "no Nomad address", mutate: func(o *Options) { o.NomadRPCAddress = "" }, wantErr: "nomad.url"},
		{name: "Nomad by name", mutate: func(o *Options) { o.NomadRPCAddress = "studio.tail1234.ts.net:4647" }, wantErr: "tailnet IP"},
		{name: "Nomad on the LAN", mutate: func(o *Options) { o.NomadRPCAddress = "192.168.1.10:4647" }, wantErr: "tailnet IP"},
		{name: "untagged", mutate: func(o *Options) { o.TailscaleTags = []string{"grove-vm"} }, wantErr: "not a Tailscale ACL tag"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := isolatedOptions()
			tt.mutate(&opts)
			client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}}
			r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{isolatedPool()}}, Options: opts}

			plan, err := r.Plan(context.Background())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.Creates) != 0 {
				t.Fatalf("blocked pool must not get VMs, got %+v", plan.Creates)
			}
			if len(plan.Blocked) != 1 || plan.Blocked[0].Pool != "linux" || !strings.Contains(plan.Blocked[0].Reason, tt.wantErr) {
				t.Fatalf("want linux blocked with %q, got %+v", tt.wantErr, plan.Blocked)
			}
			if err := plan.BlockedError(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("BlockedError() = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestPlan_IsolatedPool_TailnetIPv6Allowed(t *testing.T) {
	opts := isolatedOptions()
	opts.NomadRPCAddress = "[fd7a:115c:a1e0::1]:4647"
	if problem := opts.isolationProblem(isolatedPool()); problem != "" {
		t.Fatalf("tailnet IPv6 address should be usable, got %q", problem)
	}
}

func TestPlan_BlockedIsolatedPool_DeletesUnisolatedVMsKeepsIsolatedOnes(t *testing.T) {
	shared := orchard.VM{
		Name:   "linux-mac1-0",
		Status: "running",
		Labels: map[string]string{LabelWorkerPin: "mac1"},
	}
	isolated := orchard.VM{
		Name:         "linux-mac2-0",
		Status:       "running",
		Labels:       map[string]string{LabelWorkerPin: "mac2"},
		Softnet:      true,
		SoftnetBlock: []string{"out @host"},
	}
	client := &fakeOrchardClient{
		workers: []orchard.Worker{onlineWorker("mac1"), onlineWorker("mac2")},
		vms:     []orchard.VM{shared, isolated},
	}
	opts := isolatedOptions()
	opts.TailscaleAuthKey = ""
	r := &Reconciler{Client: client, Spec: &Spec{Pools: []Pool{isolatedPool()}}, Options: opts}

	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Creates) != 0 {
		t.Fatalf("blocked pool must not get VMs, got %+v", plan.Creates)
	}
	if len(plan.Deletes) != 1 || plan.Deletes[0].Name != "linux-mac1-0" {
		t.Fatalf("want only the unisolated VM deleted, got %+v", plan.Deletes)
	}
	if !strings.HasPrefix(plan.Deletes[0].Reason, "pool blocked: ") {
		t.Fatalf("delete reason should say the pool is blocked, got %q", plan.Deletes[0].Reason)
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

func TestTailscaleAuthKeyForVM(t *testing.T) {
	for in, want := range map[string]string{
		"tskey-auth-abc":                     "tskey-auth-abc",
		"tskey-client-k1-secret":             "tskey-client-k1-secret?ephemeral=true&preauthorized=true",
		"tskey-client-k1-secret?ephemeral=1": "tskey-client-k1-secret?ephemeral=1",
	} {
		if got := tailscaleAuthKeyForVM(in); got != want {
			t.Errorf("tailscaleAuthKeyForVM(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildStartupScript_Isolated(t *testing.T) {
	opts := isolatedOptions()
	opts.TailscaleTags = []string{"tag:grove-vm", "tag:ci"}
	script := buildStartupScript(isolatedPool(), "mac1", "linux-mac1-0", opts)

	// Each step must come after the previous one: meta first, then the fence, then the tailnet
	// login, then the RPC address, and only then the Nomad restart that picks it all up.
	last := -1
	for _, want := range []string{
		"grove_meta_header <<GROVE_META",
		"grove_priv rm -f /etc/resolv.conf",
		"nameserver 1.1.1.1\nnameserver 8.8.8.8\nGROVE_RESOLV_CONF",
		"grove_priv tee /usr/local/sbin/grove-egress-fence",
		"grove_priv tee /etc/systemd/system/docker.service.d/grove-egress-fence.conf",
		"grove_priv /usr/local/sbin/grove-egress-fence",
		"grove_priv tailscale up --reset --auth-key='tskey-auth-abc' --hostname=\"$grove_vm\" \\\n" +
			"  --advertise-tags='tag:grove-vm,tag:ci' --accept-dns=false --accept-routes=false --ssh=false \\\n" +
			"  --shields-up --timeout=120s",
		"grove_priv tee /etc/nomad.d/grove-rpc.hcl",
		`servers = ["100.64.0.10:4647"]`,
		"grove_priv systemctl restart nomad",
	} {
		at := strings.Index(script, want)
		if at < 0 {
			t.Fatalf("isolated startup script missing %q:\n%s", want, script)
		}
		if at < last {
			t.Fatalf("isolated startup script has %q out of order:\n%s", want, script)
		}
		last = at
	}

	for _, unwanted := range []string{"--ssh --accept-routes", "--accept-routes\n", "--authkey="} {
		if strings.Contains(script, unwanted) {
			t.Errorf("isolated startup script must not contain %q:\n%s", unwanted, script)
		}
	}
}

// TestBuildStartupScript_IsolatedRunsThroughFakeSudo executes the isolated startup script as an
// unprivileged guest user against stub commands and checks every privileged step it takes, in
// order, and the files it writes.
func TestBuildStartupScript_IsolatedRunsThroughFakeSudo(t *testing.T) {
	fakeBin := t.TempDir()
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
	writeExecutable("tailscale", "#!/bin/sh\nexit 64\n") // only ever run through sudo
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
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"GROVE_TEST_SUDO_LOG="+logPath,
		"GROVE_TEST_ROOT="+root,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated startup script failed through fake sudo: %v\n%s", err, output)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read sudo log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(log)), "\n")
	want := []string{
		"mkdir -p /etc/nomad.d",
		"tee /etc/nomad.d/grove-meta.hcl",
		"tee -a /etc/nomad.d/grove-meta.hcl",
		"rm -f /etc/resolv.conf",
		"tee /etc/resolv.conf",
		"mkdir -p /usr/local/sbin /etc/systemd/system/docker.service.d",
		"tee /usr/local/sbin/grove-egress-fence",
		"chmod 0755 /usr/local/sbin/grove-egress-fence",
		"tee /etc/systemd/system/docker.service.d/grove-egress-fence.conf",
		"systemctl daemon-reload",
		"/usr/local/sbin/grove-egress-fence",
		"systemctl enable --now tailscaled",
		"tailscale up --reset --auth-key=tskey-auth-abc --hostname=linux-mac1-0 --advertise-tags=tag:grove-vm " +
			"--accept-dns=false --accept-routes=false --ssh=false --shields-up --timeout=120s",
		"tee /etc/nomad.d/grove-rpc.hcl",
		"systemctl restart nomad",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("privileged steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for path, want := range map[string]string{
		"/etc/resolv.conf":                                             "nameserver 1.1.1.1\nnameserver 8.8.8.8\n",
		"/usr/local/sbin/grove-egress-fence":                           egressFenceScript,
		"/etc/nomad.d/grove-rpc.hcl":                                   "client {\n  servers = [\"100.64.0.10:4647\"]\n}\n",
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
		fakeBin := t.TempDir()
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
			cmd := exec.Command("/bin/sh", "-c", egressFenceScript)
			cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "GROVE_TEST_CHAINS="+chains)
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
		if got, want := readChain("GROVE-EGRESS"), "-o docker0 -j RETURN\n-o br-+ -j RETURN\n-o tailscale0"+reject+
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
