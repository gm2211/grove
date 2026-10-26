package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/pki"
)

func TestFleetOptions_MutualTLSEnrolsVMsWithTheCAAndNoSharedNodeKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	files, _, err := pki.EnsureNomad(dir, pki.ServerSANs{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Nomad: config.Endpoint{
		URL: "https://127.0.0.1:4646", Token: "mgmt",
		CACert: files.CA, ClientCert: files.CLI, ClientKey: files.CLIKey,
		NodeCert: files.Node, NodeKey: files.NodeKey,
	}}
	opts, err := fleetOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := os.ReadFile(files.CA)
	if opts.NodeTLS == nil || opts.NodeTLS.CA != string(ca) {
		t.Fatalf("NodeTLS = %+v, want the CA certificate", opts.NodeTLS)
	}
	if opts.NodeCerts == nil || opts.NodeIntros == nil || opts.Nodes == nil || opts.NodePools == nil || opts.NodeDrainer == nil {
		t.Fatalf("enrolment is not fully wired: %+v", opts)
	}
	if _, err := opts.NodeCerts.IssueNode("linux-mac1-0", time.Now(), pki.NodeCertValidity); err != nil {
		t.Fatalf("the issuer can't sign with grove's CA: %v", err)
	}

	// Without the CA key there is no enrolment, so no VM could ever register: fail loudly.
	if err := os.Remove(files.CAKey); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetOptions(cfg); err == nil {
		t.Fatal("fleetOptions must fail without the CA key")
	}
}

func TestNodePoolFor_OnlySharedPoolsUnderMutualTLS(t *testing.T) {
	tls := &config.Config{Nomad: config.Endpoint{NodeCert: "/x/node.pem"}}
	shared := fleet.Pool{Name: "linux"}
	sealed := fleet.Pool{Name: "sealed", Network: fleet.NetworkIsolated}
	if got := nodePoolFor(tls, shared); got != "linux" {
		t.Errorf("shared pool under mTLS: %q, want linux", got)
	}
	if got := nodePoolFor(tls, sealed); got != "" {
		t.Errorf("sealed pool: %q, want none", got)
	}
	if got := nodePoolFor(&config.Config{}, shared); got != "" {
		t.Errorf("without mTLS: %q, want none (its VMs stay in the default node pool)", got)
	}
}
