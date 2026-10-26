package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA is the broker's own certificate authority. A job VM trusts it so the broker can read and
// rewrite the requests the VM sends to the inject hosts. It is name-constrained to those hosts:
// even with its key, nobody can use it to impersonate any other site to a VM.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
	now    func() time.Time
}

const (
	caFile    = "broker-ca.pem"
	caKeyFile = "broker-ca-key.pem"
	caLife    = 5 * 365 * 24 * time.Hour
	leafLife  = 7 * 24 * time.Hour
)

// LoadOrCreateCA loads the CA kept in dir, creating it (key mode 0600, dir 0700) on first use.
// Keeping it across restarts means a VM that already trusts it keeps working when the broker
// restarts. With dir "" the CA lives in memory only.
func LoadOrCreateCA(dir string) (*CA, error) {
	if dir != "" {
		certPEM, certErr := os.ReadFile(filepath.Join(dir, caFile))
		keyPEM, keyErr := os.ReadFile(filepath.Join(dir, caKeyFile))
		if certErr == nil && keyErr == nil {
			return parseCA(certPEM, keyPEM)
		}
		if !errors.Is(certErr, os.ErrNotExist) || !errors.Is(keyErr, os.ErrNotExist) {
			return nil, fmt.Errorf("broker CA in %s is incomplete or unreadable (cert: %v, key: %v); delete both files to start over", dir, certErr, keyErr)
		}
	}
	ca, keyPEM, err := newCA(time.Now())
	if err != nil {
		return nil, err
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(filepath.Join(dir, caKeyFile), keyPEM, 0o600); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(filepath.Join(dir, caFile), ca.certPEM, 0o644); err != nil {
			return nil, err
		}
	}
	return ca, nil
}

func newCA(now time.Time) (*CA, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                randomSerial(),
		Subject:                     pkix.Name{CommonName: "grove broker CA"},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(caLife),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         InjectHosts(),
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	ca, err := parseCA(certPEM, keyPEM)
	return ca, keyPEM, err
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("broker CA: not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("broker CA: %w", err)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("broker CA key: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("broker CA: key does not match certificate")
	}
	return &CA{cert: cert, certPEM: certPEM, key: key, leaves: map[string]*tls.Certificate{}, now: time.Now}, nil
}

// CertPEM is the CA certificate a job VM installs as trusted.
func (c *CA) CertPEM() []byte { return append([]byte(nil), c.certPEM...) }

// Leaf returns a certificate for host, one of InjectHosts, minting it on first use and again when
// it is a day from expiry.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leaves[host]; ok && now.Add(24*time.Hour).Before(l.Leaf.NotAfter) {
		return l, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(leafLife)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: leaf}
	c.leaves[host] = l
	return l, nil
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
