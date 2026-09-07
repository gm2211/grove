// Package wire is the one place `grove serve` asks for concrete Orchard/Nomad clients, so it
// doesn't need to know whether those concrete implementations exist yet.
//
// TODO(orchard/nomad clients): another agent is implementing the concrete clients concurrently
// (orchard.New(url, token) over github.com/cirruslabs/orchard/pkg/client, nomad.New(url, token)
// over github.com/hashicorp/nomad/api). As of this writing neither constructor exists yet, so
// both functions below return an error instead of failing the build. Once they land, replace the
// bodies with:
//
//	return orchard.New(cfg.URL, cfg.Token)
//	return nomad.New(cfg.URL, cfg.Token)
package wire

import (
	"fmt"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// NewOrchardClient builds the concrete Orchard client from an endpoint config.
func NewOrchardClient(cfg config.Endpoint) (orchard.Client, error) {
	return nil, fmt.Errorf("wire: orchard.New(%q, ...) not implemented yet (see internal/wire/wire.go TODO)", cfg.URL)
}

// NewNomadClient builds the concrete Nomad client from an endpoint config.
func NewNomadClient(cfg config.Endpoint) (nomad.Client, error) {
	return nil, fmt.Errorf("wire: nomad.New(%q, ...) not implemented yet (see internal/wire/wire.go TODO)", cfg.URL)
}
