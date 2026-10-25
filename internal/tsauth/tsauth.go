// Package tsauth authenticates grove API callers by their Tailscale identity instead of a
// grove-issued token.
//
// A request that reached `grove serve` over the tailnet comes from a Tailscale IP that tailscaled
// already authenticated with WireGuard keys. `tailscale whois` turns that address into the node
// (and its tags) and the user who owns it, so grove never has to mint, store or rotate a secret
// for that caller. The identity becomes grove scopes through two sources, unioned:
//
//   - the tailnet policy file, as an app capability grant (CapabilityName), so the policy lives
//     next to the rest of the tailnet's ACLs:
//
//     "grants": [{"src": ["tag:argos"], "dst": ["tag:grove"],
//     "app": {"github.com/gm2211/grove": [{"scopes": ["read", "dispatch"]}]}}]
//
//   - config.yaml's server.tailscale.grants, for tailnets whose policy file the operator doesn't
//     want to touch.
//
// Nothing here trusts a request header: only the TCP peer address, and only when it is a
// Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48). A request proxied in from localhost
// (`tailscale serve`, a reverse proxy) is not a tailnet peer and never authenticates this way.
package tsauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/config"
)

// CapabilityName is the tailnet policy app-capability grove reads scopes from.
const CapabilityName = "github.com/gm2211/grove"

// cacheTTL bounds how long a whois answer is reused. A tailnet policy change (or a node losing a
// tag) takes effect within this long; short enough to be safe, long enough that a polling UI is
// not one `tailscale whois` exec per request.
const cacheTTL = 30 * time.Second

// errorCacheTTL is how long a failed lookup is remembered, so a stream of requests from an
// address tailscaled doesn't know can't turn into a stream of execs.
const errorCacheTTL = 5 * time.Second

var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// Identity is the part of a whois answer grove uses.
type Identity struct {
	// StableID is the node's stable id (survives renames), e.g. "nXXXXXXCNTRL".
	StableID string
	// Node is the node's MagicDNS name without the trailing dot, e.g. "argos.tail1234.ts.net".
	Node string
	// Login is the owning user's login name; empty for a tagged node (tags replace the user).
	Login string
	Tags  []string
	// CapScopes are the scopes the tailnet policy grants this node through CapabilityName.
	CapScopes []string
}

// WhoIs resolves a tailnet address to the identity behind it.
type WhoIs interface {
	WhoIs(ctx context.Context, addr netip.Addr) (Identity, error)
}

// Runner is the one method of install.Runner this package needs; kept local so tsauth doesn't
// import the installer.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// CLIWhoIs asks the local tailscaled through `tailscale whois --json`. The CLI is the one
// interface every Tailscale build exposes (Linux tailscaled, the standalone macOS app, Homebrew),
// so grove needs no Tailscale library and no knowledge of where each build puts its socket.
type CLIWhoIs struct {
	Runner Runner
	Bin    string
}

// whoisResponse is the subset of `tailscale whois --json` (apitype.WhoIsResponse) grove reads.
type whoisResponse struct {
	Node struct {
		StableID string   `json:"StableID"`
		Name     string   `json:"Name"`
		Tags     []string `json:"Tags"`
	} `json:"Node"`
	UserProfile struct {
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
	CapMap map[string][]json.RawMessage `json:"CapMap"`
}

func (c CLIWhoIs) WhoIs(ctx context.Context, addr netip.Addr) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stdout, stderr, err := c.Runner.Run(ctx, c.Bin, "whois", "--json", addr.String())
	if err != nil {
		return Identity{}, fmt.Errorf("tailscale whois %s: %w: %s", addr, err, strings.TrimSpace(stderr))
	}
	return parseWhoIs([]byte(stdout))
}

func parseWhoIs(data []byte) (Identity, error) {
	var resp whoisResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return Identity{}, fmt.Errorf("parse tailscale whois: %w", err)
	}
	id := Identity{
		StableID: resp.Node.StableID,
		Node:     strings.TrimSuffix(resp.Node.Name, "."),
		Tags:     append([]string(nil), resp.Node.Tags...),
	}
	if id.StableID == "" && id.Node == "" {
		return Identity{}, errors.New("tailscale whois: answer names no node")
	}
	// A tagged node's UserProfile is the synthetic "tagged-devices" user: the tags are its
	// identity, not whoever happened to register it.
	if len(id.Tags) == 0 {
		id.Login = resp.UserProfile.LoginName
	}
	for _, raw := range resp.CapMap[CapabilityName] {
		var grant struct {
			Scopes []string `json:"scopes"`
		}
		if json.Unmarshal(raw, &grant) == nil {
			id.CapScopes = append(id.CapScopes, grant.Scopes...)
		}
	}
	return id, nil
}

// Result is an authenticated tailnet caller: a stable principal id, a display name and the
// scopes it was granted. Callers still validate Scopes against their own scope set.
type Result struct {
	ID   string
	Name string
	// Node is the node's MagicDNS name, e.g. "mac-mini-1.tail1234.ts.net".
	Node   string
	Tags   []string
	Scopes []string
}

// Authenticator maps a request's peer address to a Result. The zero value is unusable; build
// one with New.
type Authenticator struct {
	whois  WhoIs
	grants []config.TailscaleGrant
	now    func() time.Time

	mu    sync.Mutex
	cache map[netip.Addr]cacheEntry
}

type cacheEntry struct {
	at  time.Time
	id  Identity
	err error
}

// New builds an Authenticator over whois with the config.yaml grants.
func New(whois WhoIs, grants []config.TailscaleGrant) *Authenticator {
	return &Authenticator{whois: whois, grants: grants, now: time.Now, cache: map[netip.Addr]cacheEntry{}}
}

// TailnetAddr returns the peer IP of remoteAddr (an http.Request.RemoteAddr) when it is a
// Tailscale address.
func TailnetAddr(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	if tailscaleV4.Contains(addr) || tailscaleV6.Contains(addr) {
		return addr, true
	}
	return netip.Addr{}, false
}

// Authenticate resolves remoteAddr. ok is false when the peer is not on the tailnet, tailscaled
// can't place it, or nothing grants it any scope.
func (a *Authenticator) Authenticate(ctx context.Context, remoteAddr string) (Result, bool) {
	if a == nil {
		return Result{}, false
	}
	addr, ok := TailnetAddr(remoteAddr)
	if !ok {
		return Result{}, false
	}
	id, err := a.lookup(ctx, addr)
	if err != nil {
		return Result{}, false
	}
	scopes := a.scopesFor(id)
	if len(scopes) == 0 {
		return Result{}, false
	}
	key := id.StableID
	if key == "" {
		key = id.Node
	}
	name := id.Node
	if id.Login != "" {
		name = id.Login + " on " + id.Node
	}
	return Result{ID: "tailscale:" + key, Name: name, Node: id.Node, Tags: append([]string(nil), id.Tags...), Scopes: scopes}, true
}

func (a *Authenticator) lookup(ctx context.Context, addr netip.Addr) (Identity, error) {
	now := a.now()
	a.mu.Lock()
	entry, hit := a.cache[addr]
	a.mu.Unlock()
	if hit {
		ttl := cacheTTL
		if entry.err != nil {
			ttl = errorCacheTTL
		}
		if now.Sub(entry.at) < ttl {
			return entry.id, entry.err
		}
	}
	id, err := a.whois.WhoIs(ctx, addr)
	a.mu.Lock()
	a.cache[addr] = cacheEntry{at: now, id: id, err: err}
	a.mu.Unlock()
	return id, err
}

// scopesFor unions the policy-file capability scopes with every matching config.yaml grant,
// sorted and de-duplicated.
func (a *Authenticator) scopesFor(id Identity) []string {
	set := map[string]struct{}{}
	for _, s := range id.CapScopes {
		set[s] = struct{}{}
	}
	for _, g := range a.grants {
		if matches(g.Match, id) {
			for _, s := range g.Scopes {
				set[s] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// matches reports whether a config.yaml grant selector names id: "tag:x" matches a node tag,
// anything with an "@" a user login, anything else the node's stable id. Never the node's name:
// a device's owner can rename it, so a name is not an identity.
func matches(selector string, id Identity) bool {
	selector = strings.TrimSpace(selector)
	switch {
	case selector == "":
		return false
	case strings.HasPrefix(selector, "tag:"):
		for _, t := range id.Tags {
			if t == selector {
				return true
			}
		}
		return false
	case strings.Contains(selector, "@"):
		return id.Login != "" && strings.EqualFold(id.Login, selector)
	default:
		return id.StableID != "" && id.StableID == selector
	}
}
