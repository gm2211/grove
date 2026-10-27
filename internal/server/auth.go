package server

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/tsauth"
)

// PeerAuthenticator authenticates a request by its TCP peer address; *tsauth.Authenticator is the
// implementation.
type PeerAuthenticator interface {
	Authenticate(ctx context.Context, remoteAddr string) (tsauth.Result, bool)
}

type principalContextKey struct{}

func principalFromContext(ctx context.Context) Principal {
	p, _ := ctx.Value(principalContextKey{}).(Principal)
	return p
}

func principalHasScope(p Principal, scope string) bool {
	for _, candidate := range p.Scopes {
		if candidate == ScopeOperator || candidate == scope {
			return true
		}
		// The per-kind scopes each pass the route-level "may dispatch at all" gate;
		// principalCanDispatchKind then decides which kind.
		if scope == ScopeDispatch && (candidate == ScopeBuild || candidate == ScopeAgent || candidate == ScopeShell) {
			return true
		}
	}
	return false
}

// principalCanDispatchKind decides whether p may submit a job of kind. ScopeDispatch grants build
// and agent only; shell needs ScopeShell or ScopeOperator (see the scope constants' doc comment).
func principalCanDispatchKind(p Principal, kind dispatch.Kind) bool {
	if principalHasScope(p, ScopeOperator) {
		return true
	}
	switch kind {
	case dispatch.KindBuild:
		return hasExactScope(p, ScopeDispatch) || hasExactScope(p, ScopeBuild)
	case dispatch.KindAgent:
		return hasExactScope(p, ScopeDispatch) || hasExactScope(p, ScopeAgent)
	case dispatch.KindShell:
		return hasExactScope(p, ScopeShell)
	}
	return false
}

func hasExactScope(p Principal, scope string) bool {
	for _, candidate := range p.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

func requiredScope(r *http.Request) string {
	path := r.URL.Path
	if path == "/whoami" {
		return ScopeRead
	}
	// /github/ is operator-only for reads too: its status names the repositories and the window
	// the control plane is currently willing to clone privately.
	if path == "/access" || strings.HasPrefix(path, "/access/") || path == "/secrets" || strings.HasPrefix(path, "/secrets/") || strings.HasPrefix(path, "/join/approve/") ||
		path == "/join/pending" ||
		strings.HasPrefix(path, "/vms/") || strings.HasPrefix(path, "/workers/") ||
		strings.HasPrefix(path, "/github/") {
		return ScopeOperator
	}
	if path == "/broker/entries" {
		return ScopeBroker
	}
	if r.Method == http.MethodPost && path == "/jobs" ||
		r.Method == http.MethodDelete && strings.HasPrefix(path, "/jobs/") ||
		r.Method == http.MethodPut && strings.HasPrefix(path, "/jobs/") && strings.HasSuffix(path, "/credentials") ||
		r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel") {
		return ScopeDispatch
	}
	return ScopeRead
}

// withMiddleware wraps next with (in order, outside-in) request logging, same-origin CORS,
// cross-origin request rejection, and Bearer token auth.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.withLogging(s.withCORS(withCrossOriginGuard(s.withAuth(next))))
}

// withCrossOriginGuard rejects state-changing requests a browser sent from another site
// (Sec-Fetch-Site, or an Origin that doesn't match Host). A tailnet peer is authenticated by its
// address alone, so without this any web page open in a browser on a granted machine could POST
// /jobs as that machine. Requests without those headers (the CLI, argos, curl) pass untouched.
func withCrossOriginGuard(next http.Handler) http.Handler {
	return http.NewCrossOriginProtection().Handler(next)
}

// tailnetHostTrusted reports whether r was addressed to a name the control plane is actually
// reached by on the tailnet: an IP literal, localhost, a MagicDNS name (short, or under .ts.net),
// or the host of the configured server URL. Anything else is a page that pointed its own domain
// at grove's address (DNS rebinding), whose requests would otherwise carry the browser machine's
// Tailscale identity.
func tailnetHostTrusted(r *http.Request, serverURL string) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "localhost" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	if u, err := url.Parse(serverURL); err == nil && u.Hostname() != "" {
		return strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), host)
	}
	return false
}

// withAuth requires "Authorization: Bearer <token>" matching Options.Token or a device credential,
// or — with no Authorization header at all, and not on the local socket (ListenLocal) — a tailnet
// peer whose Tailscale identity is granted scopes (Options.Tailscale). Nothing configured means every /api/v1 request is refused with 503,
// unless a test set Options.InsecureNoAuth. /healthz and the public join endpoints stay reachable
// unauthenticated.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicJoin := r.Method == http.MethodPost && r.URL.Path == "/join/requests" ||
			r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/join/requests/")
		if r.URL.Path == "/healthz" || publicJoin {
			next.ServeHTTP(w, r)
			return
		}
		if s.opts.Token == "" && s.opts.AccessStore == nil && s.opts.Tailscale == nil {
			if !s.opts.InsecureNoAuth {
				http.Error(w, "grove has no authentication configured; refusing", http.StatusServiceUnavailable)
				return
			}
			p := Principal{ID: "development", Name: "unauthenticated", Scopes: []string{ScopeOperator}}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p)))
			return
		}
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		var principal Principal
		switch {
		case strings.HasPrefix(auth, prefix):
			// A presented token is always the identity, even from a tailnet peer: a bad one is a 401,
			// never a silent fall-through to whatever the peer's Tailscale identity would grant.
			token := strings.TrimPrefix(auth, prefix)
			if s.opts.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.opts.Token)) == 1 {
				principal = Principal{ID: "operator", Name: "control-plane operator", Scopes: []string{ScopeOperator}}
			} else if s.opts.AccessStore != nil {
				principal, _ = s.opts.AccessStore.Authenticate(token)
			}
		case viaLocalSocket(r.Context()):
			// The local socket has no Tailscale identity: a token or nothing.
		case auth == "" && s.opts.Tailscale != nil:
			if !tailnetHostTrusted(r, s.opts.ServerURL) {
				http.Error(w, "tailnet identity is only accepted on the control plane's own host name", http.StatusForbidden)
				return
			}
			principal = s.tailscalePrincipal(r)
		}
		if principal.ID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !principalHasScope(principal, requiredScope(r)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	})
}

// tailscalePrincipal authenticates r by its TCP peer's Tailscale identity. Scopes grove doesn't
// know are dropped, so a typo in the tailnet policy can't become a scope by accident.
func (s *Server) tailscalePrincipal(r *http.Request) Principal {
	res, ok := s.opts.Tailscale.Authenticate(r.Context(), r.RemoteAddr)
	if !ok {
		return Principal{}
	}
	if !s.opts.TrustWorkerHosts && s.isWorkerHost(r.Context(), res.Node) {
		s.log.Warn("auth: refusing Tailscale identity of an Orchard worker host; its jobs share that identity, use its device credential",
			"node", res.Node, "principal", res.ID)
		return Principal{}
	}
	scopes := make([]string, 0, len(res.Scopes))
	for _, scope := range res.Scopes {
		if _, known := validAccessScopes[scope]; known && scope != ScopeBroker {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) == 0 {
		return Principal{}
	}
	return Principal{ID: res.ID, Name: res.Name, Scopes: scopes, Tags: res.Tags}
}

// withCORS allows only same-origin requests: it reflects Origin back (so a browser-hosted UI on
// the same origin can read responses) but never sets a wildcard, so cross-origin pages get no
// CORS headers at all and the browser blocks the read.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && sameOrigin(origin, r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// statusRecorder captures the status code a handler wrote, while still forwarding Flush so
// streaming handlers (SSE, chunked logs) keep working through the middleware chain.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start))
	})
}
