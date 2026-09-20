package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
	}
	return false
}

func requiredScope(r *http.Request) string {
	path := r.URL.Path
	if path == "/whoami" {
		return ScopeRead
	}
	if strings.HasPrefix(path, "/access/") || strings.HasPrefix(path, "/join/approve/") ||
		strings.HasPrefix(path, "/vms/") || strings.HasPrefix(path, "/workers/") {
		return ScopeOperator
	}
	if r.Method == http.MethodPost && path == "/jobs" ||
		r.Method == http.MethodDelete && strings.HasPrefix(path, "/jobs/") ||
		r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel") {
		return ScopeDispatch
	}
	return ScopeRead
}

// withMiddleware wraps next with (in order, outside-in) request logging, same-origin CORS, and
// Bearer token auth.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.withLogging(s.withCORS(s.withAuth(next)))
}

// withAuth requires "Authorization: Bearer <token>" matching Options.Token, unless no token is
// configured (allow-all, already warned about at startup) or the request is for /healthz, which
// stays reachable to unauthenticated monitoring probes.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicJoin := r.Method == http.MethodPost && r.URL.Path == "/join/requests" ||
			r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/join/requests/")
		if r.URL.Path == "/healthz" || publicJoin {
			next.ServeHTTP(w, r)
			return
		}
		if s.opts.Token == "" && s.opts.AccessStore == nil {
			p := Principal{ID: "development", Name: "unauthenticated", Scopes: []string{ScopeOperator}}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p)))
			return
		}
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, prefix)
		var principal Principal
		if s.opts.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.opts.Token)) == 1 {
			principal = Principal{ID: "operator", Name: "control-plane operator", Scopes: []string{ScopeOperator}}
		} else if s.opts.AccessStore != nil {
			principal, _ = s.opts.AccessStore.Authenticate(token)
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

// withCORS allows only same-origin requests: it reflects Origin back (so a browser-hosted UI on
// the same origin can read responses) but never sets a wildcard, so cross-origin pages get no
// CORS headers at all and the browser blocks the read.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && sameOrigin(origin, r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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
