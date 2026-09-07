// Package wire is the one place `grove serve` asks for concrete Orchard/Nomad clients, so it
// doesn't need to know how those concrete implementations are constructed.
package wire

import (
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// NewOrchardClient builds the concrete Orchard client from an endpoint config.
func NewOrchardClient(cfg config.Endpoint) (orchard.Client, error) {
	return orchard.New(cfg.URL, cfg.Token)
}

// NewNomadClient builds the concrete Nomad client from an endpoint config.
func NewNomadClient(cfg config.Endpoint) (nomad.Client, error) {
	return nomad.New(cfg.URL, cfg.Token)
}
