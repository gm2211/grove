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
func TestNomadMutualTLS_RealNomad(t *testing.T) {
	bin := os.Getenv("GROVE_NOMAD_BIN")
	if bin == "" {
		t.Skip("GROVE_NOMAD_BIN not set")
	}
	root := t.TempDir()
	files, _, err := pki.EnsureNomad(filepath.Join(root, "tls"), nomadServerSANs("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	otherCA, _, err := pki.EnsureNomad(filepath.Join(root, "other-ca"), pki.ServerSANs{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Cloud fingerprinters only slow a test agent down (or fail behind a proxy).
	clientExtras := write("client-extras.hcl", `client {
  options {
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }
}
`)

	start := func(name string, configs ...string) {
		t.Helper()
		args := []string{"agent"}
		for _, c := range configs {
			args = append(args, "-config", c)
		}
		cmd := exec.Command(bin, args...)
		log, err := os.Create(filepath.Join(root, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			_ = log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Logf("--- %s log ---\n%s", name, tail(data, 40))
			}
		})
	}

	serverConf, err := RenderNomadServerConfig(NomadServerConfigSpec{
		DataDir:  filepath.Join(root, "server-data"),
		BindAddr: "127.0.0.1",
		TLS:      NomadTLSFiles{CA: files.CA, Cert: files.Server, Key: files.ServerKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Nomad refuses to advertise loopback implicitly.
	start("server", write("server.hcl", serverConf), write("server-advertise.hcl", `advertise {
  http = "127.0.0.1:4646"
  rpc  = "127.0.0.1:4647"
  serf = "127.0.0.1:4648"
}
`))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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

	// grove's own client certificate gets through verify_https_client; no certificate does not.
	nc, err := nomad.NewTLS(url, mgmt, nomad.TLS{CACert: files.CA, ClientCert: files.CLI, ClientKey: files.CLIKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nc.ListNodes(ctx); err != nil {
		t.Fatalf("grove's client certificate was refused: %v", err)
	}
	if bare, err := nomad.NewTLS(url, mgmt, nomad.TLS{CACert: files.CA}); err != nil {
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
	intros, _ := nomad.AsNodeIntros(nc)
	// Right after bootstrap the server may not have its keyring yet (the install step retries too).
	var tok string
	for {
		if tok, err = intros.CreateIntroToken(ctx, SandboxClientNodeName, "", nomadSandboxIntroTTL); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	if err := os.MkdirAll(filepath.Join(root, "sandbox-data", "client"), 0o700); err != nil {
		t.Fatal(err)
	}
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
