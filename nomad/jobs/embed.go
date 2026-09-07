// Package nomadjobs embeds grove's parameterized Nomad job templates (build/agent/shell) so the
// server can register them without reading from disk at runtime. Each *.nomad.hcl file is a Go
// text/template rendered with a struct carrying at least Kind and Pool (see README.md for the
// full dispatch contract and nomadjobs_test.go for example rendering).
package nomadjobs

import "embed"

//go:embed *.nomad.hcl
var FS embed.FS
