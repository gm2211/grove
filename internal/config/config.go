// Package config loads and saves grove's user configuration
// (~/.config/grove/config.yaml by default, overridable with GROVE_CONFIG).
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is everything a grove CLI / server needs to reach the control plane.
type Config struct {
	// Orchard controller.
	Orchard Endpoint `yaml:"orchard"`
	// Nomad server.
	Nomad Endpoint `yaml:"nomad"`
	// grove server (API + UI). Token is what clients (orchestrators, MCP, UI) present.
	Server ServerConfig `yaml:"server"`
	// Artifact store: a directory on the control plane's disk.
	Artifacts ArtifactsConfig `yaml:"artifacts,omitempty"`
	// Path to fleet.yaml. Relative paths resolve against the config file's directory.
	Fleet string `yaml:"fleet"`
	// Tailscale configures worker VMs to join the tailnet on boot (see fleet.BuildStartupScript).
	// Additive: not required by any existing consumer of Config.
	Tailscale  TailscaleConfig  `yaml:"tailscale,omitempty"`
	Enrollment EnrollmentConfig `yaml:"enrollment,omitempty"`
}

type EnrollmentConfig struct {
	// IssuerToken belongs to a control-plane-only Orchard account with the narrow
	// service-account:issue-worker role. Grove uses it only after operator approval to mint one
	// worker-scoped credential per enrolled Mac.
	IssuerToken string `yaml:"issuerToken,omitempty"`
}

type TailscaleConfig struct {
	// AuthKey is no longer used: no worker VM joins the tailnet. A shared VM reaches the control
	// plane's Nomad through its host Mac's NAT, and a key in a VM's startup script was readable by
	// anything that could list Orchard's VMs. Kept so existing config files still load.
	AuthKey string `yaml:"authKey,omitempty"`
}

type Endpoint struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token,omitempty"`

	// The rest is Nomad's mutual TLS, written by `grove install --role control-plane` (see
	// internal/pki). CACert verifies the server; ClientCert/ClientKey are grove's own client
	// identity for the HTTP API. NodeCert/NodeKey are the control plane's own sandbox-host client's
	// certificate; setting them also marks Nomad as running mutual TLS, so the fleet enrols each
	// VM with a certificate of its own, signed by the CA key beside CACert (see
	// fleet.Reconciler.Enrol). All paths, never PEM contents.
	CACert     string `yaml:"caCert,omitempty"`
	ClientCert string `yaml:"clientCert,omitempty"`
	ClientKey  string `yaml:"clientKey,omitempty"`
	NodeCert   string `yaml:"nodeCert,omitempty"`
	NodeKey    string `yaml:"nodeKey,omitempty"`
}

type ServerConfig struct {
	// Listen address for `grove serve`, e.g. "100.64.0.5:6120" or "0.0.0.0:6120".
	Listen string `yaml:"listen"`
	// URL clients use to reach the server, e.g. "http://grove-cp.tailnet.ts.net:6120".
	URL   string `yaml:"url"`
	Token string `yaml:"token,omitempty"`
	// TokenKeychain names the macOS Keychain account holding a device-scoped client token.
	TokenKeychain string `yaml:"tokenKeychain,omitempty"`
	// Tailscale lets tailnet peers call the API with no token at all, authenticated by their
	// Tailscale identity (see internal/tsauth). Bearer tokens keep working alongside it.
	Tailscale ServerTailscaleConfig `yaml:"tailscale,omitempty"`
	// Socket, when set, makes `grove serve` also listen on this Unix socket, for a client on the
	// same Mac (Argos on the Studio), e.g. "/Users/Shared/grove/api.sock". Only SocketUsers may
	// connect, checked against the user the kernel reports for each connection, and every
	// request still needs a bearer token.
	Socket string `yaml:"socket,omitempty"`
	// SocketUsers are the macOS user names (or uids) allowed to connect to Socket.
	SocketUsers []string `yaml:"socketUsers,omitempty"`
}

// ServerTailscaleConfig is server.tailscale. Identity auth is on whenever the control plane has a
// tailscale CLI, but grants nothing by itself: a peer gets scopes only from the tailnet policy
// file's "github.com/gm2211/grove" app capability or from Grants below.
type ServerTailscaleConfig struct {
	// Disabled turns Tailscale identity auth off; every caller then needs a bearer token.
	Disabled bool `yaml:"disabled,omitempty"`
	// Grants maps Tailscale identities to grove scopes, in addition to the policy file.
	Grants []TailscaleGrant `yaml:"grants,omitempty"`
	// TrustWorkerHosts lets an Orchard worker's own Mac authenticate by its Tailscale identity.
	// Off by default: a shared pool's VMs reach the control plane through their host's NAT, so
	// every job on that Mac would arrive as the Mac's identity. Workers use their device
	// credential instead.
	TrustWorkerHosts bool `yaml:"trustWorkerHosts,omitempty"`
}

// TailscaleGrant gives Scopes to every tailnet peer Match names: "tag:argos" (a node tag), a
// user login such as "alice@example.com" (untagged nodes only), or a node's stable id.
type TailscaleGrant struct {
	Match  string   `yaml:"match"`
	Scopes []string `yaml:"scopes"`
}

const ServerTokenKeychainService = "com.gm2211.grove.server-client"

// BrokerTokenKeychainService and BrokerTokenKeychainAccount name the Keychain item an older grove
// kept a worker's broker credential in (only the broker scope, named after the worker). It now
// lives in a 0600 file (CredentialsDir/BrokerCredentialFile), because the broker runs as a boot-time
// LaunchDaemon with no login Keychain to read; the item is only a migration source and fallback.
const (
	BrokerTokenKeychainService = "com.gm2211.grove.broker"
	BrokerTokenKeychainAccount = "broker"
)

// ResolveServerToken returns either the control-plane token from config or an enrolled
// device's scoped token from macOS Keychain.
func ResolveServerToken(ctx context.Context, cfg *Config) (string, error) {
	if cfg.Server.TokenKeychain != "" {
		token, err := readKeychain(ctx, cfg.Server.TokenKeychain, ServerTokenKeychainService)
		if err != nil {
			return "", fmt.Errorf("read Grove device credential from Keychain: %w", err)
		}
		return token, nil
	}
	return cfg.Server.Token, nil
}

// ResolveBrokerToken returns this worker's broker credential: the 0600 file `grove setup` writes
// under CredentialsDir, else (a worker set up before that, not yet re-installed) the Keychain item
// an older grove stored. A file group or world can read is refused, not skipped over.
func ResolveBrokerToken(ctx context.Context) (string, error) {
	dir, err := CredentialsDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, BrokerCredentialFile)
	token, err := readCredentialFile(path)
	switch {
	case err == nil:
		if token == "" {
			return "", fmt.Errorf("the broker credential file %s is empty; run `grove setup` on this Mac", path)
		}
		return token, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("read the broker credential: %w", err)
	}
	token, err = keychainReader(ctx, BrokerTokenKeychainAccount, BrokerTokenKeychainService)
	if err != nil {
		return "", fmt.Errorf("no broker credential at %s, and none in Keychain (run `grove setup` on this Mac to get one): %w", path, err)
	}
	if token == "" {
		return "", errors.New("the broker credential in Keychain is empty; run `grove setup` on this Mac")
	}
	return token, nil
}

// keychainReader reads a Keychain item; a variable so tests don't touch the real Keychain.
var keychainReader = readKeychain

func readKeychain(ctx context.Context, account, service string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("credential is kept in macOS Keychain, and this host is not macOS")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ResolveOperatorToken returns the control-plane credential used only for enrollment and access
// administration. It never falls back to a device credential.
func ResolveOperatorToken(cfg *Config) (string, error) {
	if cfg.Server.Token == "" {
		return "", errors.New("control-plane operator credential is not configured on this device")
	}
	return cfg.Server.Token, nil
}

// ArtifactsConfig configures the control plane's artifact store, a plain directory that only
// `grove serve` and `grove sandbox run` (both on the control plane) read and write.
type ArtifactsConfig struct {
	// Dir is the store's root directory. Empty means the default, $XDG_STATE_HOME/grove/artifacts
	// (~/.local/state/grove/artifacts), next to the job history. A relative path resolves against
	// the config file's directory.
	Dir string `yaml:"dir,omitempty"`
	// Retention bounds how long a finished job's artifacts stay in the store. Zero values take
	// the defaults (DefaultArtifactRetentionSuccess/Failure).
	Retention ArtifactRetention `yaml:"retention,omitempty"`

	// Deprecated: grove used to keep artifacts in an S3-compatible bucket (MinIO on the control
	// plane). These keys still parse, so an older config.yaml loads, but nothing reads them; see
	// LegacyKeys. `grove install --role control-plane` clears them.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Deprecated: see Endpoint.
	Bucket string `yaml:"bucket,omitempty"`
	// Deprecated: see Endpoint.
	AccessKey string `yaml:"accessKey,omitempty"`
	// Deprecated: see Endpoint.
	SecretKey string `yaml:"secretKey,omitempty"`
}

// LegacyKeys names the deprecated S3/MinIO keys set in c ("artifacts.endpoint", …), so callers
// can warn that they are ignored. Nil when there are none.
func (c ArtifactsConfig) LegacyKeys() []string {
	var keys []string
	for _, kv := range []struct{ key, value string }{
		{"artifacts.endpoint", c.Endpoint},
		{"artifacts.bucket", c.Bucket},
		{"artifacts.accessKey", c.AccessKey},
		{"artifacts.secretKey", c.SecretKey},
	} {
		if kv.value != "" {
			keys = append(keys, kv.key)
		}
	}
	return keys
}

// ClearLegacy drops the deprecated S3/MinIO keys (including the old secret key) from c.
func (c *ArtifactsConfig) ClearLegacy() {
	c.Endpoint, c.Bucket, c.AccessKey, c.SecretKey = "", "", "", ""
}

// ArtifactRetention is how long `grove serve` keeps a job's artifacts after the job finishes:
// Success for a job that succeeded, Failure for every other end (failed, canceled, lost), which
// people tend to want to look at a little longer. Durations use Go's syntax ("15m", "2h").
type ArtifactRetention struct {
	Success time.Duration `yaml:"success,omitempty"`
	Failure time.Duration `yaml:"failure,omitempty"`
}

// Default artifact retention: short on purpose, so the store never grows without bound.
const (
	DefaultArtifactRetentionSuccess = 15 * time.Minute
	DefaultArtifactRetentionFailure = time.Hour
)

// WithDefaults fills zero durations with the defaults.
func (r ArtifactRetention) WithDefaults() ArtifactRetention {
	if r.Success <= 0 {
		r.Success = DefaultArtifactRetentionSuccess
	}
	if r.Failure <= 0 {
		r.Failure = DefaultArtifactRetentionFailure
	}
	return r
}

// ErrNotFound is returned by Load when no config file exists yet.
var ErrNotFound = errors.New("grove config not found; run `grove install` or `grove config init`")

// DefaultPath returns the config file path, honouring GROVE_CONFIG.
func DefaultPath() (string, error) {
	if p := os.Getenv("GROVE_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "grove", "config.yaml"), nil
}

// Load reads the config from DefaultPath.
func Load() (*Config, string, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, "", err
	}
	cfg, err := LoadFrom(path)
	return cfg, path, err
}

// LoadFrom reads a config file from an explicit path.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Fleet != "" && !filepath.IsAbs(cfg.Fleet) {
		cfg.Fleet = filepath.Join(filepath.Dir(path), cfg.Fleet)
	}
	if cfg.Artifacts.Dir != "" && !filepath.IsAbs(cfg.Artifacts.Dir) {
		cfg.Artifacts.Dir = filepath.Join(filepath.Dir(path), cfg.Artifacts.Dir)
	}
	return &cfg, nil
}

// Save writes the config to path, creating parent directories with 0700 and the file with 0600
// because it may contain tokens.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
