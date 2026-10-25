// Package pki is the control plane's certificate authority for Nomad's mutual TLS.
//
// Without mTLS, anything that reaches the Nomad server's RPC port can register as a client node
// and be handed jobs (and their payloads). With it, the server only talks to peers holding a
// certificate this CA signed, and Nomad's role check (verify_server_hostname) only accepts
// "client.<region>.nomad" or "server.<region>.nomad" names for those roles. The CA key never
// leaves the control plane: grove signs every certificate here.
//
// The files EnsureNomad maintains under one directory:
//
//	ca.pem, ca-key.pem          the CA (10 years)
//	server.pem, server-key.pem  server.global.nomad plus the server's own addresses (2 years)
//	cli.pem, cli-key.pem        cli.global.nomad: grove's own HTTP API client (2 years)
//	node.pem, node-key.pem      client.global.nomad: what every VM's Nomad client presents (2 years)
//
// A node certificate alone does not let a VM join: the server also enforces client
// introduction, so registering needs a per-VM introduction token as well (see internal/fleet).
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 2 * 365 * 24 * time.Hour
	// RenewBefore is how close to expiry a leaf certificate is reissued by EnsureNomad (and
	// flagged by `grove doctor`).
	RenewBefore = 30 * 24 * time.Hour
)

// Nomad's fixed TLS names for the default region.
const (
	ServerName = "server.global.nomad"
	ClientName = "client.global.nomad"
	CLIName    = "cli.global.nomad"
)

// Files are the paths EnsureNomad maintains.
type Files struct {
	CA, CAKey         string
	Server, ServerKey string
	CLI, CLIKey       string
	Node, NodeKey     string
}

// NomadFiles names the files under dir.
func NomadFiles(dir string) Files {
	j := func(name string) string { return filepath.Join(dir, name) }
	return Files{
		CA: j("ca.pem"), CAKey: j("ca-key.pem"),
		Server: j("server.pem"), ServerKey: j("server-key.pem"),
		CLI: j("cli.pem"), CLIKey: j("cli-key.pem"),
		Node: j("node.pem"), NodeKey: j("node-key.pem"),
	}
}

// ServerSANs are the extra names and addresses the server certificate must cover: whatever
// grove and the VMs dial it by.
type ServerSANs struct {
	DNSNames []string
	IPs      []net.IP
}

// EnsureNomad makes dir hold a complete, current set of Files. It creates the CA when absent and
// (re)issues each leaf that is missing, signed by another CA, within RenewBefore of expiry, or (the
// server certificate) missing one of sans. changed reports whether anything was written. Keys are
// mode 0600 and dir 0700; every write is an atomic rename.
func EnsureNomad(dir string, sans ServerSANs, now time.Time) (files Files, changed bool, err error) {
	files = NomadFiles(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return files, false, fmt.Errorf("pki: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return files, false, fmt.Errorf("pki: secure %s: %w", dir, err)
	}

	ca, caKey, err := loadPair(files.CA, files.CAKey)
	switch {
	case err == nil && !ca.IsCA:
		return files, false, fmt.Errorf("pki: %s is not a CA certificate (move it aside to start over)", files.CA)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return files, false, fmt.Errorf("pki: CA in %s is unreadable (move it aside to start over): %w", dir, err)
	}
	// A missing or expiring CA is (re)created, and every leaf below is then reissued because it no
	// longer verifies against it. VMs pick the new CA up when the fleet recreates them for their
	// changed startup script.
	if err != nil || now.After(ca.NotAfter.Add(-RenewBefore)) {
		ca, caKey, err = newCA(now)
		if err != nil {
			return files, false, err
		}
		if err := writePair(files.CA, files.CAKey, ca, caKey); err != nil {
			return files, false, err
		}
		changed = true
	}

	leaves := []struct {
		cert, key string
		tmpl      func() *x509.Certificate
		covers    func(*x509.Certificate) bool
	}{
		{files.Server, files.ServerKey, func() *x509.Certificate { return serverTemplate(sans) }, func(c *x509.Certificate) bool { return coversSANs(c, sans) }},
		{files.CLI, files.CLIKey, func() *x509.Certificate { return clientTemplate(CLIName) }, nil},
		{files.Node, files.NodeKey, func() *x509.Certificate { return clientTemplate(ClientName) }, nil},
	}
	for _, l := range leaves {
		cert, _, err := loadPair(l.cert, l.key)
		if err == nil && cert.CheckSignatureFrom(ca) == nil && now.Before(cert.NotAfter.Add(-RenewBefore)) &&
			(l.covers == nil || l.covers(cert)) {
			continue
		}
		tmpl := l.tmpl()
		cert, key, err := issue(tmpl, ca, caKey, now)
		if err != nil {
			return files, false, err
		}
		if err := writePair(l.cert, l.key, cert, key); err != nil {
			return files, false, err
		}
		changed = true
	}
	return files, changed, nil
}

// Current reports whether dir already holds what EnsureNomad would leave there, without writing:
// the planner's idempotency check.
func Current(dir string, sans ServerSANs, now time.Time) bool {
	files := NomadFiles(dir)
	ca, _, err := loadPair(files.CA, files.CAKey)
	if err != nil || !ca.IsCA || now.After(ca.NotAfter.Add(-RenewBefore)) {
		return false
	}
	for _, pair := range [][2]string{{files.Server, files.ServerKey}, {files.CLI, files.CLIKey}, {files.Node, files.NodeKey}} {
		cert, _, err := loadPair(pair[0], pair[1])
		if err != nil || cert.CheckSignatureFrom(ca) != nil || !now.Before(cert.NotAfter.Add(-RenewBefore)) {
			return false
		}
		if pair[0] == files.Server && !coversSANs(cert, sans) {
			return false
		}
	}
	return true
}

// Expiry returns the NotAfter of the first certificate in path.
func Expiry(path string) (time.Time, error) {
	cert, err := loadCert(path)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}

func newCA(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: generate CA key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "grove nomad CA", Organization: []string{"grove"}},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: self-sign CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: parse CA: %w", err)
	}
	return cert, key, nil
}

func serverTemplate(sans ServerSANs) *x509.Certificate {
	dns := []string{ServerName, "localhost"}
	for _, n := range sans.DNSNames {
		if n != "" && !slices.Contains(dns, n) {
			dns = append(dns, n)
		}
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1)}
	for _, ip := range sans.IPs {
		if ip != nil && !containsIP(ips, ip) {
			ips = append(ips, ip)
		}
	}
	return &x509.Certificate{
		Subject:     pkix.Name{CommonName: ServerName},
		DNSNames:    dns,
		IPAddresses: ips,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// Servers also dial each other and act as clients of their own RPC.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
}

func clientTemplate(name string) *x509.Certificate {
	return &x509.Certificate{
		Subject:  pkix.Name{CommonName: name},
		DNSNames: []string{name, "localhost"},
		// Nomad clients also serve (their own HTTP API and alloc exec streams), so client
		// certificates carry both usages, as `nomad tls cert create -client` does.
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
}

func issue(tmpl, ca *x509.Certificate, caKey *ecdsa.PrivateKey, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: generate key for %s: %w", tmpl.Subject.CommonName, err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = now.Add(-time.Minute)
	tmpl.NotAfter = now.Add(leafValidity)
	if tmpl.NotAfter.After(ca.NotAfter) {
		tmpl.NotAfter = ca.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: sign %s: %w", tmpl.Subject.CommonName, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: parse %s: %w", tmpl.Subject.CommonName, err)
	}
	return cert, key, nil
}

func coversSANs(cert *x509.Certificate, sans ServerSANs) bool {
	for _, n := range sans.DNSNames {
		if n != "" && !slices.Contains(cert.DNSNames, n) {
			return false
		}
	}
	for _, ip := range sans.IPs {
		if ip != nil && !containsIP(cert.IPAddresses, ip) {
			return false
		}
	}
	return true
}

func containsIP(ips []net.IP, ip net.IP) bool {
	for _, have := range ips {
		if have.Equal(ip) {
			return true
		}
	}
	return false
}

func serialNumber() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("pki: serial number: %w", err)
	}
	return n, nil
}

func loadCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func loadPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := loadCert(certPath)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, nil, fmt.Errorf("%s holds no PEM EC private key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, fmt.Errorf("%s does not match %s", keyPath, certPath)
	}
	return cert, key, nil
}

func writePair(certPath, keyPath string, cert *x509.Certificate, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("pki: marshal key: %w", err)
	}
	// Key first: a crash in between leaves a key no certificate matches, which loadPair rejects,
	// so the next run reissues both.
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	return nil
}
