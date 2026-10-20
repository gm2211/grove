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
	// Deprecated: Nomad was the job scheduler before grove ran jobs itself. The key still parses,
	// so an older config.yaml loads, but nothing reads it; see LegacyKeys.
	Nomad Endpoint `yaml:"nomad,omitempty"`
	// grove server (API + UI). Token is what clients (orchestrators, MCP, UI) present.
	Server ServerConfig `yaml:"server"`
	// Artifact store: a directory on the control plane's disk.
	Artifacts ArtifactsConfig `yaml:"artifacts,omitempty"`
	// Path to fleet.yaml. Relative paths resolve against the config file's directory.
	Fleet string `yaml:"fleet"`
	// Deprecated: Tailscale held the auth key shared-network worker VMs joined the tailnet with.
	// Every pool is sealed now and no VM joins the tailnet; see LegacyKeys.
	Tailscale  TailscaleConfig  `yaml:"tailscale,omitempty"`
	Enrollment EnrollmentConfig `yaml:"enrollment,omitempty"`
}

type EnrollmentConfig struct {
	// IssuerToken belongs to a control-plane-only Orchard account with the narrow
	// service-account:issue-worker role. Grove uses it only after operator approval to mint one
	// worker-scoped credential per enrolled Mac.
	IssuerToken string `yaml:"issuerToken,omitempty"`
}

// Deprecated: see Config.Tailscale.
type TailscaleConfig struct {
	AuthKey string `yaml:"authKey,omitempty"`
}

// LegacyKeys names the deprecated top-level keys set in c ("nomad.url", "tailscale.authKey", …),
// so callers can warn that they are ignored. Nil when there are none. See also
// ArtifactsConfig.LegacyKeys.
func (c Config) LegacyKeys() []string {
	var keys []string
	if c.Nomad.URL != "" {
		keys = append(keys, "nomad.url")
	}
	if c.Nomad.Token != "" {
		keys = append(keys, "nomad.token")
	}
	if c.Tailscale.AuthKey != "" {
		keys = append(keys, "tailscale.authKey")
	}
	return keys
}

// ClearLegacy drops the deprecated top-level keys (including the old Nomad token and Tailscale
// auth key) from c.
func (c *Config) ClearLegacy() {
	c.Nomad = Endpoint{}
	c.Tailscale = TailscaleConfig{}
}

type Endpoint struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token,omitempty"`
}

type ServerConfig struct {
	// Listen address for `grove serve`, e.g. "100.64.0.5:6120" or "0.0.0.0:6120".
	Listen string `yaml:"listen"`
	// URL clients use to reach the server, e.g. "http://grove-cp.tailnet.ts.net:6120".
	URL   string `yaml:"url"`
	Token string `yaml:"token,omitempty"`
	// TokenKeychain names the macOS Keychain account holding a device-scoped client token.
	TokenKeychain string `yaml:"tokenKeychain,omitempty"`
}

const ServerTokenKeychainService = "com.gm2211.grove.server-client"

// ResolveServerToken returns either the control-plane token from config or an enrolled
// device's scoped token from macOS Keychain.
func ResolveServerToken(ctx context.Context, cfg *Config) (string, error) {
	if cfg.Server.TokenKeychain != "" {
		if runtime.GOOS != "darwin" {
			return "", errors.New("server token references macOS Keychain on a non-macOS host")
		}
		cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", cfg.Server.TokenKeychain, "-s", ServerTokenKeychainService, "-w")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("read Grove device credential from Keychain: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
	return cfg.Server.Token, nil
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
// `grove serve` on the control plane reads and writes.
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
