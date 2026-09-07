// Package config loads and saves grove's user configuration
// (~/.config/grove/config.yaml by default, overridable with GROVE_CONFIG).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is everything a grove CLI / server needs to reach the control plane.
type Config struct {
	// Orchard controller.
	Orchard Endpoint `yaml:"orchard"`
	// Nomad server.
	Nomad Endpoint `yaml:"nomad"`
	// grove server (API + UI). Token is what clients (Argos, MCP, UI) present.
	Server ServerConfig `yaml:"server"`
	// Artifact store (S3-compatible, e.g. MinIO on the control plane).
	Artifacts ArtifactsConfig `yaml:"artifacts"`
	// Path to fleet.yaml. Relative paths resolve against the config file's directory.
	Fleet string `yaml:"fleet"`
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
}

type ArtifactsConfig struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"accessKey,omitempty"`
	SecretKey string `yaml:"secretKey,omitempty"`
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
