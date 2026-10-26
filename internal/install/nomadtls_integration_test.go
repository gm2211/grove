package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/pki"
)

// TestNomadMutualTLS_RealNomad runs the server and sandbox-host client configs grove renders
// against a real Nomad agent and checks who can register. It needs a nomad binary
// (GROVE_NOMAD_BIN, which CI sets to the pinned NomadVersion) and the default ports 4646-4648,
// 4656-4658 and 4666-4678 free on 127.0.0.1; it is skipped otherwise.
//
// Every machine that used to be able to join (any process that reached 4647) is refused: no
// TLS, a certificate from another CA, and grove's own node certificate without an introduction
// token. The sandbox-host client registers once handed a token minted for its name.
// nomadTestServer is a real Nomad server running grove's rendered server config, with grove's
// management token and HTTP client.
type nomadTestServer struct {
	t      *testing.T
	bin    string
	root   string
	files  pki.Files
	nc     nomad.Client
	ctx    context.Context
	extras string // client config that keeps cloud fingerprinters out of a test agent
}

func startNomadTestServer(t *testing.T) *nomadTestServer {
	bin := os.Getenv("GROVE_NOMAD_BIN")
	if bin == "" {
		t.Skip("GROVE_NOMAD_BIN not set")
	}
	s := &nomadTestServer{t: t, bin: bin, root: t.TempDir()}
	files, _, err := pki.EnsureNomad(filepath.Join(s.root, "tls"), nomadServerSANs("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.files = files
	// Cloud fingerprinters only slow a test agent down (or fail behind a proxy).
	s.extras = s.write("client-extras.hcl", `client {
  options {
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }
}
`)

	serverConf, err := RenderNomadServerConfig(NomadServerConfigSpec{
		DataDir:  filepath.Join(s.root, "server-data"),
		BindAddr: "127.0.0.1",
		TLS:      NomadTLSFiles{CA: files.CA, Cert: files.Server, Key: files.ServerKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Nomad refuses to advertise loopback implicitly.
	s.start("server", s.write("server.hcl", serverConf), s.write("server-advertise.hcl", `advertise {
  http = "127.0.0.1:4646"
  rpc  = "127.0.0.1:4647"
  serf = "127.0.0.1:4648"
}
`))

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	s.ctx = ctx
	url := "https://127.0.0.1:4646"
	tlsFlags := []string{"-ca-cert=" + files.CA, "-client-cert=" + files.CLI, "-client-key=" + files.CLIKey}
	var mgmt string
	for mgmt == "" {
		out, err := exec.CommandContext(ctx, bin, append(append([]string{"acl", "bootstrap", "-address=" + url}, tlsFlags...), "-json")...).Output()
		if err == nil {
			var resp struct{ SecretID string }
			_ = json.Unmarshal(out, &resp)
			mgmt = resp.SecretID
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(time.Second)
	}
	s.nc, err = nomad.NewTLS(url, mgmt, nomad.TLS{CACert: files.CA, ClientCert: files.CLI, ClientKey: files.CLIKey})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *nomadTestServer) write(name, content string) string {
	s.t.Helper()
	path := filepath.Join(s.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return path
}

// nomadTestAgent is a running Nomad agent.
type nomadTestAgent struct {
	cmd  *exec.Cmd
	stop func()
}

// start runs a Nomad agent with configs until the test ends (or stop is called).
func (s *nomadTestServer) start(name string, configs ...string) *nomadTestAgent {
	s.t.Helper()
	args := []string{"agent"}
	for _, c := range configs {
		args = append(args, "-config", c)
	}
	cmd := exec.Command(s.bin, args...)
	log, err := os.OpenFile(filepath.Join(s.root, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start %s: %v", name, err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			_ = log.Close()
		})
	}
	s.t.Cleanup(func() {
		stop()
		if s.t.Failed() {
			data, _ := os.ReadFile(log.Name())
			s.t.Logf("--- %s log ---\n%s", name, tail(data, 40))
		}
	})
	return &nomadTestAgent{cmd: cmd, stop: stop}
}

// agentLog returns what the agent started as name has logged so far.
func (s *nomadTestServer) agentLog(name string) string {
	data, _ := os.ReadFile(filepath.Join(s.root, name+".log"))
	return string(data)
}

// introToken mints an introduction token, retrying while a just-bootstrapped server has no
// keyring yet (the install step retries too).
func (s *nomadTestServer) introToken(name, pool string, ttl time.Duration) string {
	s.t.Helper()
	intros, _ := nomad.AsNodeIntros(s.nc)
	for {
		tok, err := intros.CreateIntroToken(s.ctx, name, pool, ttl)
		if err == nil {
			return tok
		}
		if s.ctx.Err() != nil {
			s.t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
}

// node returns the registered node named name, if any (none while the server is restarting).
func (s *nomadTestServer) node(name string) (nomad.NodeSummary, bool) {
	s.t.Helper()
	purger, _ := nomad.AsNodePurger(s.nc)
	nodes, err := purger.ListNodeSummaries(s.ctx)
	if err != nil {
		return nomad.NodeSummary{}, false
	}
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return nomad.NodeSummary{}, false
}

// waitReady waits for the node named name to be ready.
func (s *nomadTestServer) waitReady(name string) nomad.NodeSummary {
	s.t.Helper()
	for {
		if n, ok := s.node(name); ok && n.Status == "ready" {
			return n
		}
		if s.ctx.Err() != nil {
			s.t.Fatalf("node %s never became ready", name)
		}
		time.Sleep(time.Second)
	}
}

// TestNomadMutualTLS_RealNomad runs the server and sandbox-host client configs grove renders
// against a real Nomad agent and checks who can register. It needs a nomad binary
// (GROVE_NOMAD_BIN, which CI sets to the pinned NomadVersion) and the default ports 4646-4648,
// 4656-4658 and 4666-4690 free on 127.0.0.1; it is skipped otherwise.
//
// Every machine that used to be able to join (any process that reached 4647) is refused: no
// TLS, a certificate from another CA, and grove's own node certificate without an introduction
// token. The sandbox-host client registers once handed a token minted for its name.
func TestNomadMutualTLS_RealNomad(t *testing.T) {
	srv := startNomadTestServer(t)
	root, files, nc, ctx, write, start, clientExtras := srv.root, srv.files, srv.nc, srv.ctx, srv.write, srv.start, srv.extras
	otherCA, _, err := pki.EnsureNomad(filepath.Join(root, "other-ca"), pki.ServerSANs{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// grove's own client certificate gets through verify_https_client; no certificate does not.
	if _, err := nc.ListNodes(ctx); err != nil {
		t.Fatalf("grove's client certificate was refused: %v", err)
	}
	if bare, err := nomad.NewTLS("https://127.0.0.1:4646", "", nomad.TLS{CACert: files.CA}); err != nil {
		t.Fatal(err)
	} else if _, err := bare.ListNodes(ctx); err == nil {
		t.Fatal("the HTTP API answered a caller with no client certificate")
	}

	rogue := func(name string, port int, tls string) string {
		return write(name+".hcl", fmt.Sprintf(`name = %q
data_dir = %q
bind_addr = "127.0.0.1"
ports {
  http = %d
  rpc = %d
  serf = %d
}
advertise {
  http = "127.0.0.1:%d"
  rpc = "127.0.0.1:%d"
  serf = "127.0.0.1:%d"
}
client {
  enabled = true
  servers = ["127.0.0.1:4647"]
  meta {
    pool = "linux"
  }
}
%s`, name, filepath.Join(root, name+"-data"), port, port+1, port+2, port, port+1, port+2, tls))
	}
	tlsBlock := func(ca, cert, key string) string {
		return fmt.Sprintf("tls {\n  rpc = true\n  ca_file = %q\n  cert_file = %q\n  key_file = %q\n  verify_server_hostname = true\n}\n", ca, cert, key)
	}
	start("rogue-no-tls", rogue("rogue-no-tls", 4666, ""), clientExtras)
	start("rogue-other-ca", rogue("rogue-other-ca", 4670, tlsBlock(otherCA.CA, otherCA.Node, otherCA.NodeKey)), clientExtras)
	start("rogue-no-intro", rogue("rogue-no-intro", 4674, tlsBlock(files.CA, files.Node, files.NodeKey)), clientExtras)

	sandboxConf, err := RenderNomadSandboxClientConfig(NomadSandboxClientConfigSpec{
		DataDir: filepath.Join(root, "sandbox-data"),
		Server:  "127.0.0.1:4647",
		TLS:     NomadTLSFiles{CA: files.CA, Cert: files.Node, Key: files.NodeKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	tok := srv.introToken(SandboxClientNodeName, "", nomadSandboxIntroTTL)
	write(filepath.Join("sandbox-data", "client", "intro_token.jwt"), tok)
	start("sandbox", write("sandbox-client.hcl", sandboxConf), clientExtras)

	var registered []nomad.Node
	for {
		nodes, err := nc.ListNodes(ctx)
		if err == nil {
			registered = nodes
			if len(nodes) > 0 && nodes[0].Status == "ready" {
				break
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("the sandbox-host client never registered; %d nodes", len(registered))
		}
		time.Sleep(time.Second)
	}
	// Give the rogues as long again to fail before checking they did.
	time.Sleep(15 * time.Second)
	nodes, err := nc.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	if len(nodes) != 1 || nodes[0].Name != SandboxClientNodeName || nodes[0].Meta["grove_sandbox_host"] != "true" {
		t.Fatalf("registered nodes = %v, want only %s", names, SandboxClientNodeName)
	}
}

func tail(data []byte, lines int) []byte {
	parts := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return []byte(strings.Join(func() []string {
		out := make([]string, len(parts))
		for i, p := range parts {
			out[i] = string(p)
		}
		return out
	}(), "\n"))
}

// vmConfigs writes a VM's client config, certificate and (when token is set) introduction token,
// laid out as a VM's startup script and enrolment leave them, and returns the agent's configs.
func (s *nomadTestServer) vmConfigs(name string, port int, pool string, cert pki.NodeCert, token string) []string {
	dir := "vm-" + name
	certPath := s.write(dir+"/node.pem", cert.Cert)
	keyPath := s.write(dir+"/node-key.pem", cert.Key)
	if token != "" {
		s.write(dir+"/data/client/intro_token.jwt", token)
	}
	conf := s.write(dir+"/client.hcl", fmt.Sprintf(`name = %q
data_dir = %q
bind_addr = "127.0.0.1"
ports {
  http = %d
  rpc = %d
  serf = %d
}
advertise {
  http = "127.0.0.1:%d"
  rpc = "127.0.0.1:%d"
  serf = "127.0.0.1:%d"
}
client {
  enabled = true
  servers = ["127.0.0.1:4647"]
  node_pool = %q
}
tls {
  rpc = true
  ca_file = %q
  cert_file = %q
  key_file = %q
  verify_server_hostname = true
}
`, name, filepath.Join(s.root, dir, "data"), port, port+1, port+2, port, port+1, port+2, pool,
		s.files.CA, certPath, keyPath))
	return []string{conf, s.extras}
}

// startVMClient runs a VM's Nomad client (see vmConfigs), logging to name.log.
func (s *nomadTestServer) startVMClient(name string, port int, pool string, cert pki.NodeCert, token string) *nomadTestAgent {
	return s.start(name, s.vmConfigs(name, port, pool, cert, token)...)
}

// TestNomadPerVMEnrolment_RealNomad proves what the fleet's enrolment relies on, against a real
// Nomad: a VM's own short-lived certificate (pki.CA.IssueNode) passes the server's role check;
// the introduction token pins the node pool, so a VM cannot register in another pool's; and a
// renewed certificate written over the old one is what the client presents next, while one
// nobody renewed no longer gets a client back in once it has expired.
func TestNomadPerVMEnrolment_RealNomad(t *testing.T) {
	srv := startNomadTestServer(t)
	pools, _ := nomad.AsNodePools(srv.nc)
	if err := pools.EnsureNodePool(srv.ctx, "linux", "test"); err != nil {
		t.Fatal(err)
	}
	if err := pools.EnsureNodePool(srv.ctx, "linux", "test"); err != nil {
		t.Fatalf("EnsureNodePool on an existing pool: %v", err)
	}
	ca, err := pki.LoadCA(srv.files.CA, srv.files.CAKey)
	if err != nil {
		t.Fatal(err)
	}

	const shortLife = 40 * time.Second
	vm := srv.vmConfigs
	issue := func(name string, life time.Duration) pki.NodeCert {
		c, err := ca.IssueNode(name, time.Now(), life)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	first0 := issue("linux-mac1-0", shortLife)
	first1 := issue("linux-mac1-1", shortLife)
	renewed := srv.start("renewed", vm("linux-mac1-0", 4680, "linux", first0, srv.introToken("linux-mac1-0", "linux", time.Hour))...)
	expiring := srv.start("expiring", vm("linux-mac1-1", 4684, "linux", first1, srv.introToken("linux-mac1-1", "linux", time.Hour))...)
	// A token for the linux pool does not let a client register in another one.
	srv.start("wrong-pool", vm("linux-mac1-2", 4688, "default", issue("linux-mac1-2", time.Hour), srv.introToken("linux-mac1-2", "linux", time.Hour))...)
	issuedAt := time.Now()

	if n := srv.waitReady("linux-mac1-0"); n.NodePool != "linux" {
		t.Fatalf("node pool = %q, want linux", n.NodePool)
	}
	srv.waitReady("linux-mac1-1")

	// A connection that is already up outlives its certificate, so expiry bites only on the next
	// handshake. Wait out the first certificates, then renew linux-mac1-0 the way the enrolment
	// exec does (new files over the old, then SIGHUP): the reload drops its connections, and the
	// handshake that follows can only succeed on the new certificate.
	time.Sleep(time.Until(issuedAt.Add(shortLife + 5*time.Second)))
	fresh := issue("linux-mac1-0", time.Hour)
	srv.write("vm-linux-mac1-0/node-key.pem", fresh.Key)
	srv.write("vm-linux-mac1-0/node.pem", fresh.Cert)
	if err := renewed.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	for {
		log := srv.agentLog("renewed")
		if _, after, ok := strings.Cut(log, "Reloading configuration"); ok && strings.Contains(after, "node registration complete") {
			if strings.Contains(after, "expired certificate") {
				t.Fatal("the renewed client handshook with its expired certificate")
			}
			break
		}
		if srv.ctx.Err() != nil {
			t.Fatal("the renewed client never re-registered after its reload")
		}
		time.Sleep(time.Second)
	}
	// linux-mac1-1 was never renewed: once its client restarts, it is locked out.
	expiring.stop()
	srv.start("expiring", vm("linux-mac1-1", 4684, "linux", first1, "")...)
	for {
		n0, _ := srv.node("linux-mac1-0")
		n1, _ := srv.node("linux-mac1-1")
		if n1.Status == "down" {
			if n0.Status != "ready" {
				t.Fatalf("the renewed client is %q", n0.Status)
			}
			break
		}
		if srv.ctx.Err() != nil {
			t.Fatalf("linux-mac1-1 is still %q on an expired certificate", n1.Status)
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(srv.agentLog("expiring"), "expired certificate") {
		t.Fatal("linux-mac1-1 went down, but not for its expired certificate")
	}
	if n, ok := srv.node("linux-mac1-2"); ok {
		t.Fatalf("a client registered in pool %q with a token bound to linux", n.NodePool)
	}
}

// TestNomadRollCallControls_RealNomad proves the two Nomad behaviours the roll call and the
// read-only node token rely on: a VM's node token can watch its own node but drain nothing, and a
// node the control plane has drained stays ineligible after its client restarts (where a purged
// live node would simply register again).
func TestNomadRollCallControls_RealNomad(t *testing.T) {
	srv := startNomadTestServer(t)
	pools, _ := nomad.AsNodePools(srv.nc)
	if err := pools.EnsureNodePool(srv.ctx, "linux", "test"); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadCA(srv.files.CA, srv.files.CAKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.IssueNode("linux-mac1-0", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	vm := srv.startVMClient("linux-mac1-0", 4680, "linux", cert, srv.introToken("linux-mac1-0", "linux", time.Hour))
	n := srv.waitReady("linux-mac1-0")

	tokens, _ := nomad.AsNodeTokens(srv.nc)
	tok, err := tokens.CreateNodeToken(srv.ctx, "linux-mac1-0")
	if err != nil {
		t.Fatal(err)
	}
	asVM := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(srv.ctx, srv.bin, args...)
		// The VM's shutdown script talks to its own client's plain HTTP API on loopback.
		cmd.Env = append(os.Environ(), "NOMAD_ADDR=http://127.0.0.1:4680", "NOMAD_TOKEN="+tok.SecretID)
		return cmd.CombinedOutput()
	}
	if out, err := asVM("node", "status", "-self", "-json"); err != nil {
		t.Fatalf("the node token can't watch its own node: %v\n%s", err, out)
	}
	if out, err := asVM("node", "drain", "-self", "-enable", "-yes", "-deadline", "1m"); err == nil || !strings.Contains(string(out), "Permission denied") {
		t.Fatalf("want the node token refused a drain (Permission denied), got %v:\n%s", err, out)
	}
	if out, err := asVM("node", "eligibility", "-disable", n.ID); err == nil || !strings.Contains(string(out), "Permission denied") {
		t.Fatalf("want the node token refused an eligibility change (Permission denied), got %v:\n%s", err, out)
	}

	if err := srv.nc.DrainNode(srv.ctx, n.ID, true, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	vm.stop()
	srv.startVMClient("linux-mac1-0", 4680, "linux", cert, "")
	time.Sleep(15 * time.Second)
	got := srv.waitReady("linux-mac1-0")
	if got.Eligible {
		t.Fatal("a drained node came back eligible after its client restarted")
	}
}
