package dispatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// maxToolchainBytes caps the canonical encoding of a Toolchain: it rides in Nomad dispatch meta.
const maxToolchainBytes = 1024

// toolchainHashPrefix versions the hash so a change to the canonical form can't collide with the
// old one.
const toolchainHashPrefix = "grove-toolchain/1\n"

// Toolchain is Environment.Toolchain: exact versions of the native tools a macOS job runs with.
// The set of tools is fixed; a version is exact ("1.99.1"), never a channel or a range.
type Toolchain struct {
	Rust   *RustToolchain `json:"rust,omitempty"`
	Node   *ToolVersion   `json:"node,omitempty"`
	Python *ToolVersion   `json:"python,omitempty"`
}

// RustToolchain is Toolchain.Rust.
type RustToolchain struct {
	Version string `json:"version"`
	// Components is a subset of allowedRustComponents.
	Components []string `json:"components,omitempty"`
}

// ToolVersion is Toolchain.Node and Toolchain.Python.
type ToolVersion struct {
	Version string `json:"version"`
}

var (
	rustVersionPattern   = regexp.MustCompile(`^1\.\d+\.\d+$`)
	nodeVersionPattern   = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	pythonVersionPattern = regexp.MustCompile(`^3\.\d+\.\d+$`)

	allowedRustComponents = map[string]bool{"clippy": true, "rustfmt": true, "rust-src": true, "llvm-tools": true}
)

// strictDecode unmarshals data into v and refuses a field v doesn't have, so an unknown tool
// ("go") or option is a 400 and not a silently dropped request.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (t *Toolchain) UnmarshalJSON(data []byte) error {
	type plain Toolchain
	return strictDecode(data, (*plain)(t))
}

func (r *RustToolchain) UnmarshalJSON(data []byte) error {
	type plain RustToolchain
	return strictDecode(data, (*plain)(r))
}

func (v *ToolVersion) UnmarshalJSON(data []byte) error {
	type plain ToolVersion
	return strictDecode(data, (*plain)(v))
}

// ValidateToolchain checks t: at least one tool, exact versions only, known rust components, and
// a canonical encoding of at most 1 KiB.
func ValidateToolchain(t *Toolchain) error {
	if t == nil {
		return nil
	}
	if t.Rust == nil && t.Node == nil && t.Python == nil {
		return fmt.Errorf("toolchain names no tool (rust, node or python)")
	}
	if t.Rust != nil {
		if !rustVersionPattern.MatchString(t.Rust.Version) {
			return fmt.Errorf("toolchain rust version %q must be exact, like 1.99.1 (no channel, range or partial version)", t.Rust.Version)
		}
		for _, c := range t.Rust.Components {
			if !allowedRustComponents[c] {
				return fmt.Errorf("toolchain rust component %q is not one of clippy, rustfmt, rust-src, llvm-tools", c)
			}
		}
	}
	if t.Node != nil && !nodeVersionPattern.MatchString(t.Node.Version) {
		return fmt.Errorf("toolchain node version %q must be exact, like 22.11.0 (no major-only version or alias)", t.Node.Version)
	}
	if t.Python != nil && !pythonVersionPattern.MatchString(t.Python.Version) {
		return fmt.Errorf("toolchain python version %q must be exact, like 3.12.7", t.Python.Version)
	}
	// Bound the input before canonicalising it: a request can carry any number of components.
	if t.Rust != nil && len(t.Rust.Components) > len(allowedRustComponents)*4 {
		return fmt.Errorf("toolchain rust lists too many components")
	}
	if c := t.canonical(); len(c) > maxToolchainBytes {
		return fmt.Errorf("toolchain encodes to %d bytes, over the %d byte limit", len(c), maxToolchainBytes)
	}
	return nil
}

// canonicalToolchain fixes the key order of the canonical encoding (alphabetical).
type canonicalToolchain struct {
	Node   *ToolVersion   `json:"node,omitempty"`
	Python *ToolVersion   `json:"python,omitempty"`
	Rust   *RustToolchain `json:"rust,omitempty"`
}

// canonical is t re-marshalled deterministically: keys in alphabetical order, components sorted
// and unique, no whitespace. It is the only form that reaches a worker, so what a caller wrote
// (key order, duplicates, unknown spellings) can't differ between two equal requests.
func (t *Toolchain) canonical() string {
	c := canonicalToolchain{Node: t.Node, Python: t.Python}
	if t.Rust != nil {
		set := make(map[string]struct{}, len(t.Rust.Components))
		for _, comp := range t.Rust.Components {
			set[comp] = struct{}{}
		}
		comps := make([]string, 0, len(set))
		for comp := range set {
			comps = append(comps, comp)
		}
		sort.Strings(comps)
		c.Rust = &RustToolchain{Version: t.Rust.Version, Components: comps}
	}
	// json.Marshal escapes <, > and & as < etc.; none can pass validation, so the form is
	// plain ASCII. Marshal of these types can't fail.
	b, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("dispatch: marshal toolchain: %v", err))
	}
	return string(b)
}

// hash is the hex sha256 of toolchainHashPrefix + canonical: the key a worker caches the installed
// toolchain under.
func (t *Toolchain) hash() string {
	sum := sha256.Sum256([]byte(toolchainHashPrefix + t.canonical()))
	return hex.EncodeToString(sum[:])
}
