package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/broker"
)

// testCA makes a self-signed certificate from tmpl, PEM-encoded.
func testCA(t *testing.T, tmpl *x509.Certificate) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.SerialNumber = big.NewInt(1)
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// brokerCATemplate is what internal/broker's CA looks like; each case below bends one thing.
func brokerCATemplate(hosts []string) *x509.Certificate {
	return &x509.Certificate{
		Subject:                     pkix.Name{CommonName: "grove broker CA"},
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         hosts,
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
}

// runBrokerScript runs brokerScript as a Linux guest would, with the gateway the stub ip reports
// and the stub curl serving ca. It returns the Nomad meta file it wrote ("" for none) and the
// script's output.
func runBrokerScript(t *testing.T, gateway, ca string) (meta, output string) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	fakeBin := t.TempDir()
	dir := t.TempDir()
	caFile := filepath.Join(dir, "served.pem")
	if err := os.WriteFile(caFile, []byte(ca), 0o644); err != nil {
		t.Fatal(err)
	}
	route := ""
	if gateway != "" {
		route = "echo 'default via " + gateway + " dev enp0s1 proto dhcp src 192.168.64.5 metric 100'\n"
	}
	for name, body := range map[string]string{
		"uname": "#!/bin/sh\necho Linux\n",
		"ip":    "#!/bin/sh\n[ \"$*\" = \"-4 route show default\" ] || exit 64\n" + route,
		"sleep": "#!/bin/sh\nexit 0\n",
		// The stub serves the file only at the broker's URL on the gateway.
		"curl": `#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    --noproxy|--max-time|--max-filesize) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
echo "$url" >> "$GROVE_TEST_DIR/curl.log"
[ "$url" = "http://$GROVE_TEST_GATEWAY:3128/ca.pem" ] || exit 7
[ -s "$GROVE_TEST_CA" ] || exit 7
cp "$GROVE_TEST_CA" "$out"
`,
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	metaDir := filepath.Join(dir, "nomad.d")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -eu\ngrove_priv() { \"$@\"; }\n" + strings.ReplaceAll(brokerScript(), "/etc/nomad.d", metaDir)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"),
		"GROVE_TEST_DIR="+dir, "GROVE_TEST_CA="+caFile, "GROVE_TEST_GATEWAY="+gateway)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("broker script failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(metaDir, "grove-broker.hcl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data), string(out)
}

// A shared VM advertises its worker's broker, and the broker's CA, only when that CA is exactly
// what internal/broker makes: a forged or looser CA, or none at all, leaves it without a broker.
func TestBrokerScript_AdvertisesOnlyGrovesBrokerCA(t *testing.T) {
	real, err := broker.LoadOrCreateCA("")
	if err != nil {
		t.Fatal(err)
	}
	realPEM := string(real.CertPEM())

	meta, out := runBrokerScript(t, "192.168.64.1", realPEM)
	lines := strings.Split(strings.TrimSpace(realPEM), "\n")
	body := strings.Join(lines[1:len(lines)-1], "")
	want := "client {\n  meta {\n    grove_broker    = \"http://192.168.64.1:3128\"\n    grove_broker_ca = \"" + body + "\"\n  }\n}\n"
	if meta != want {
		t.Fatalf("meta =\n%s\nwant:\n%s\noutput:\n%s", meta, want, out)
	}

	hosts := broker.InjectHosts()
	wider := brokerCATemplate(append(append([]string{}, hosts...), "evil.example"))
	noIPExclusion := brokerCATemplate(hosts)
	noIPExclusion.ExcludedIPRanges = noIPExclusion.ExcludedIPRanges[:1]
	notCritical := brokerCATemplate(hosts)
	notCritical.PermittedDNSDomainsCritical = false
	unconstrained := brokerCATemplate(nil)
	unconstrained.ExcludedIPRanges = nil
	notCA := brokerCATemplate(hosts)
	notCA.IsCA, notCA.MaxPathLenZero, notCA.KeyUsage = false, false, x509.KeyUsageDigitalSignature
	deeper := brokerCATemplate(hosts)
	deeper.MaxPathLenZero = false
	for name, ca := range map[string]string{
		"an unconstrained CA":       testCA(t, unconstrained),
		"one more permitted host":   testCA(t, wider),
		"IPv6 not excluded":         testCA(t, noIPExclusion),
		"constraints not critical":  testCA(t, notCritical),
		"not a CA":                  testCA(t, notCA),
		"a CA that may sign CAs":    testCA(t, deeper),
		"two certificates":          realPEM + realPEM,
		"a key alongside":           realPEM + "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n",
		"not PEM":                   "<html>hello</html>\n",
		"nothing served, no broker": "",
	} {
		t.Run(name, func(t *testing.T) {
			meta, out := runBrokerScript(t, "192.168.64.1", ca)
			if meta != "" {
				t.Fatalf("advertised a broker for %s:\n%s\noutput:\n%s", name, meta, out)
			}
			if !strings.Contains(out, "gets no broker") && !strings.Contains(out, "no credential broker") {
				t.Errorf("no word on why there is no broker:\n%s", out)
			}
		})
	}

	// A forged CA with exactly the broker's constraints passes, and can do no more than the
	// real one: that is what the check is for.
	if meta, out := runBrokerScript(t, "192.168.64.1", testCA(t, brokerCATemplate(hosts))); meta == "" {
		t.Errorf("a CA with the broker's exact constraints was refused:\n%s", out)
	}

	if meta, _ := runBrokerScript(t, "", realPEM); meta != "" {
		t.Errorf("no default route, yet a broker:\n%s", meta)
	}
}

// A shared pool's startup script finds the broker after its home fence and before Nomad restarts,
// so the node's first registration already advertises it; a sealed pool's never looks for one.
func TestBuildStartupScript_FindsBrokerBeforeNomadRestarts(t *testing.T) {
	for _, pool := range []Pool{{Name: "linux"}, {Name: "macos"}} {
		script := buildStartupScript(pool, "mac1", pool.Name+"-mac1-0", Options{})
		fenceAt := strings.Index(script, "jobs reach the internet, not home")
		brokerAt := strings.Index(script, "# --- credential broker ---")
		restartAt := strings.LastIndex(script, "com.grove.nomad")
		if !pool.MacOS() {
			restartAt = strings.LastIndex(script, "systemctl restart nomad")
		}
		if fenceAt < 0 || brokerAt < fenceAt || restartAt < brokerAt {
			t.Errorf("%s: broker block missing or out of order (fence %d, broker %d, restart %d)", pool.Name, fenceAt, brokerAt, restartAt)
		}
	}
	for _, pool := range []Pool{{Name: "linux", Network: NetworkIsolated}, {Name: "macos", Network: NetworkIsolated}} {
		if script := buildStartupScript(pool, "mac1", pool.Name+"-mac1-0", Options{}); strings.Contains(script, "credential broker") {
			t.Errorf("%s: a sealed VM can't reach the broker and must not look for one", pool.Name)
		}
	}
}
