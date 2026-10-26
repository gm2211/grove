package broker

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func caPool(t *testing.T, ca *CA) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}
	return pool
}

func TestCALeavesVerifyForInjectHosts(t *testing.T) {
	ca, err := LoadOrCreateCA("")
	if err != nil {
		t.Fatal(err)
	}
	pool := caPool(t, ca)
	for _, host := range InjectHosts() {
		leaf, err := ca.Leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: pool}); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		again, _ := ca.Leaf(host)
		if again != leaf {
			t.Fatalf("%s: leaf not cached", host)
		}
	}
}

// Even someone holding the CA key can't use it against any other site: the CA is name-constrained.
func TestCANameConstrained(t *testing.T) {
	ca, err := LoadOrCreateCA("")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for _, name := range []string{"example.com", "github.com.evil.test", "pypi.org"} {
		tmpl := &x509.Certificate{
			SerialNumber: randomSerial(),
			Subject:      pkix.Name{CommonName: name},
			DNSNames:     []string{name},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(der)
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: caPool(t, ca)}); err == nil {
			t.Fatalf("a certificate for %s from the broker CA verified", name)
		}
	}
}

func TestLoadOrCreateCAPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "broker")
	a, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.CertPEM(), b.CertPEM()) {
		t.Fatal("restart minted a new CA; VMs that trust the old one would break")
	}
	st, err := os.Stat(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, want 0600", st.Mode().Perm())
	}
	if err := os.Remove(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil {
		t.Fatal("a CA with its key missing was silently replaced")
	}
}
