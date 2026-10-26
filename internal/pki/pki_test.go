package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestEnsureNomadCreatesVerifiableSet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	now := time.Now()
	sans := ServerSANs{DNSNames: []string{"cp.tail1234.ts.net"}, IPs: []net.IP{net.ParseIP("100.64.0.5")}}
	files, changed, err := EnsureNomad(dir, sans, now)
	if err != nil || !changed {
		t.Fatalf("EnsureNomad = changed %v, err %v", changed, err)
	}

	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	for _, key := range []string{files.CAKey, files.ServerKey, files.CLIKey, files.NodeKey} {
		info, err := os.Stat(key)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v; want 0600", key, info.Mode().Perm(), err)
		}
	}

	ca, err := loadCert(files.CA)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	server, err := loadCert(files.Server)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ServerName, "cp.tail1234.ts.net", "100.64.0.5", "127.0.0.1"} {
		if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("server cert does not verify for %s: %v", name, err)
		}
	}
	for path, name := range map[string]string{files.CLI: CLIName, files.Node: ClientName} {
		cert, err := loadCert(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Errorf("%s does not verify as a client for %s: %v", path, name, err)
		}
	}
	if _, err := tls.LoadX509KeyPair(files.Node, files.NodeKey); err != nil {
		t.Errorf("node pair does not load: %v", err)
	}
}

func TestEnsureNomadIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sans := ServerSANs{DNSNames: []string{"cp"}}
	files, _, err := EnsureNomad(dir, sans, now)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(files.Node)
	if !Current(dir, sans, now) {
		t.Fatal("Current = false right after EnsureNomad")
	}
	if Current(dir, ServerSANs{DNSNames: []string{"other"}}, now) {
		t.Fatal("Current = true for a server name the certificate lacks")
	}
	_, changed, err := EnsureNomad(dir, sans, now.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("second EnsureNomad = changed %v, err %v; want unchanged", changed, err)
	}
	after, _ := os.ReadFile(files.Node)
	if string(before) != string(after) {
		t.Fatal("node certificate was reissued without cause")
	}
}

func TestEnsureNomadReissuesServerForNewAddress(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	files, _, err := EnsureNomad(dir, ServerSANs{DNSNames: []string{"old-host"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	caBefore, _ := os.ReadFile(files.CA)
	nodeBefore, _ := os.ReadFile(files.Node)
	_, changed, err := EnsureNomad(dir, ServerSANs{DNSNames: []string{"new-host"}}, now)
	if err != nil || !changed {
		t.Fatalf("EnsureNomad = changed %v, err %v; want the server cert reissued", changed, err)
	}
	server, _ := loadCert(files.Server)
	if err := server.VerifyHostname("new-host"); err != nil {
		t.Fatalf("server cert not reissued for the new host: %v", err)
	}
	caAfter, _ := os.ReadFile(files.CA)
	nodeAfter, _ := os.ReadFile(files.Node)
	if string(caBefore) != string(caAfter) || string(nodeBefore) != string(nodeAfter) {
		t.Fatal("a server address change must not touch the CA or the node certificate")
	}
}

func TestEnsureNomadRenewsNearExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	files, _, err := EnsureNomad(dir, ServerSANs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	expiry, _ := Expiry(files.Node)
	_, changed, err := EnsureNomad(dir, ServerSANs{}, expiry.Add(-RenewBefore+time.Hour))
	if err != nil || !changed {
		t.Fatalf("EnsureNomad near expiry = changed %v, err %v; want renewal", changed, err)
	}
	renewed, _ := Expiry(files.Node)
	if !renewed.After(expiry) {
		t.Fatalf("node cert expiry %v not after %v", renewed, expiry)
	}
}

func TestEnsureNomadReissuesLeavesForAnotherCA(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	files, _, err := EnsureNomad(dir, ServerSANs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Someone replaces the CA (for example after a compromise): every leaf must follow it.
	if err := os.Remove(files.CA); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(files.CAKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureNomad(dir, ServerSANs{}, now); err != nil {
		t.Fatal(err)
	}
	ca, _ := loadCert(files.CA)
	for _, path := range []string{files.Server, files.CLI, files.Node} {
		cert, _ := loadCert(path)
		if err := cert.CheckSignatureFrom(ca); err != nil {
			t.Errorf("%s not reissued under the new CA: %v", path, err)
		}
	}
}

func TestEnsureNomadRefusesACorruptCA(t *testing.T) {
	dir := t.TempDir()
	files := NomadFiles(dir)
	if err := os.WriteFile(files.CA, []byte("not a cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.CAKey, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureNomad(dir, ServerSANs{}, time.Now()); err == nil {
		t.Fatal("EnsureNomad silently replaced an unreadable CA")
	}
}

func TestIssueNode_IsTheVMsOwnShortLivedClientCertificate(t *testing.T) {
	now := time.Now()
	files, _, err := EnsureNomad(filepath.Join(t.TempDir(), "tls"), ServerSANs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := LoadCA(files.CA, files.CAKey)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := ca.IssueNode("linux-mac1-0", now, NodeCertValidity)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(nc.Cert))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != ClientName || !slices.Contains(cert.DNSNames, ClientName) {
		t.Errorf("Nomad's role check needs %s, got CN %q, DNS %v", ClientName, cert.Subject.CommonName, cert.DNSNames)
	}
	if !slices.Contains(cert.DNSNames, "linux-mac1-0") || !slices.Equal(cert.Subject.OrganizationalUnit, []string{"linux-mac1-0"}) {
		t.Errorf("the VM's name must be in the certificate, got DNS %v, OU %v", cert.DNSNames, cert.Subject.OrganizationalUnit)
	}
	if !nc.NotAfter.Equal(cert.NotAfter) || cert.NotAfter.After(now.Add(NodeCertValidity)) {
		t.Errorf("NotAfter = %v (reported %v), want at most a day out", cert.NotAfter, nc.NotAfter)
	}
	caCert, _ := loadCert(files.CA)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("does not verify as a client of grove's CA: %v", err)
	}
	if _, err := tls.X509KeyPair([]byte(nc.Cert), []byte(nc.Key)); err != nil {
		t.Errorf("key does not match: %v", err)
	}
	other, _ := ca.IssueNode("linux-mac1-0", now, NodeCertValidity)
	if other.Key == nc.Key {
		t.Error("every issue must get a fresh key")
	}
	if _, err := ca.IssueNode("", now, NodeCertValidity); err == nil {
		t.Error("a node certificate without a VM name must be refused")
	}
	if _, err := LoadCA(files.Node, files.NodeKey); err == nil {
		t.Error("a leaf certificate must not load as a CA")
	}
}
