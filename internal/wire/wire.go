// Package wire is the one place `grove serve` asks for concrete Orchard and artifact-store clients, so it
// doesn't need to know how those concrete implementations are constructed.
package wire

import (
	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/orchard"
)

// NewOrchardClient builds the concrete Orchard client from an endpoint config.
func NewOrchardClient(cfg config.Endpoint) (orchard.Client, error) {
	return orchard.New(cfg.URL, cfg.Token)
}

// NewArtifactsClient builds the concrete artifact store (a directory on this machine) from config.
// Returns artifacts.ErrNotConfigured (not fatal) when no store directory can be determined.
func NewArtifactsClient(cfg config.ArtifactsConfig) (artifacts.Client, error) {
	return artifacts.New(cfg)
}
