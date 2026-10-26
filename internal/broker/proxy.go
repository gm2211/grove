package broker

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Port is where a worker's broker listens on the VM bridge address. A job VM's firewall allows
// this one port on its gateway and nothing else on the host.
const Port = 3128

const (
	dialTimeout       = 30 * time.Second
	handshakeTimeout  = 30 * time.Second
	readHeaderTimeout = 30 * time.Second
	// tunnelIdle closes a tunnel or intercepted connection that has moved no bytes either way for
	// this long. Agent sessions keep streams open for minutes, so it is generous.
	tunnelIdle = 15 * time.Minute
	// DefaultMaxConnsPerSource caps the requests and tunnels one address may have open at once.
	DefaultMaxConnsPerSource = 512
)

// Proxy is the HTTP proxy a job VM's HTTPS_PROXY/HTTP_PROXY point at.
//
//   - CONNECT to an inject host on port 443 is intercepted: the broker terminates TLS with a leaf
//     from CA, swaps placeholders for real tokens (inject), forwards the request to the real host,
//     and masks the tokens in the response (stripper).
//   - Any other CONNECT is a plain byte tunnel, but only to a public address (Dialer).
//   - A plain-HTTP request is forwarded to a public address with nothing injected; plain HTTP to an
//     inject host is refused.
//   - GET /ca.pem serves the CA certificate, GET /healthz reports the entry count and whether
//     the latest sync worked.
type Proxy struct {
	Store  *Store
	CA     *CA
	Dialer *Dialer
	// Resync, when set, is called before a request with an unknown placeholder is refused, and the
	// lookup tried once more: a job can start using its placeholder before the next scheduled sync.
	// It should rate-limit itself.
	Resync func(ctx context.Context)
	// Synced, when set, reports whether the broker's latest sync with the control plane worked;
	// GET /healthz shows it, so `grove doctor` can tell a broker that can't sync.
	Synced func() bool
	// Upstream sends intercepted and plain-HTTP requests; nil means a transport dialing through
	// Dialer with the system's roots. Tests replace it.
	Upstream          http.RoundTripper
	MaxConnsPerSource int
	Log               *slog.Logger

	initOnce sync.Once
	upstream http.RoundTripper
	mu       sync.Mutex
	open     map[netip.Addr]int
}

func (p *Proxy) init() {
	p.initOnce.Do(func() {
		if p.Dialer == nil {
			p.Dialer = &Dialer{}
		}
		if p.Log == nil {
			p.Log = slog.Default()
		}
		if p.MaxConnsPerSource <= 0 {
			p.MaxConnsPerSource = DefaultMaxConnsPerSource
		}
		p.open = map[netip.Addr]int{}
		p.upstream = p.Upstream
		if p.upstream == nil {
			p.upstream = &http.Transport{
				Proxy:               nil, // never chain to the host's own proxy settings
				DialContext:         p.Dialer.DialContext,
				ForceAttemptHTTP2:   true,
				TLSHandshakeTimeout: handshakeTimeout,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 16,
			}
		}
	})
}

// Serve answers proxy requests on l until ctx is done.
func (p *Proxy) Serve(ctx context.Context, l net.Listener) error {
	p.init()
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       tunnelIdle,
		ErrorLog:          slog.NewLogLogger(p.Log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	src := remoteAddr(r.RemoteAddr)
	if !p.acquire(src) {
		http.Error(w, "broker: too many open connections from this address", http.StatusTooManyRequests)
		return
	}
	defer p.release(src)

	switch {
	case r.Method == http.MethodConnect:
		p.connect(w, r, src)
	case r.URL.IsAbs():
		p.forwardPlain(w, r)
	case r.URL.Path == "/ca.pem" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(p.CA.CertPEM())
	case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		health := map[string]any{"ok": true, "entries": p.Store.Len()}
		if p.Synced != nil {
			health["synced"] = p.Synced()
		}
		json.NewEncoder(w).Encode(health)
	default:
		http.NotFound(w, r)
	}
}

func (p *Proxy) acquire(src netip.Addr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open[src] >= p.MaxConnsPerSource {
		return false
	}
	p.open[src]++
	return true
}

func (p *Proxy) release(src netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open[src]--; p.open[src] <= 0 {
		delete(p.open, src)
	}
}

func remoteAddr(s string) netip.Addr {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

func isInjectHost(host string) bool {
	for k := range kindHosts {
		if hostAllowed(k, host) {
			return true
		}
	}
	return false
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, src netip.Addr) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "broker: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	host = normalizeHost(host)
	if isInjectHost(host) && port == "443" {
		p.intercept(w, host, src)
		return
	}
	upstream, err := p.Dialer.DialContext(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		p.Log.Warn("broker: tunnel refused", "src", src, "dest", r.Host, "err", err)
		status := http.StatusBadGateway
		if errors.Is(err, ErrNotPublic) {
			status = http.StatusForbidden
		}
		http.Error(w, "broker: "+err.Error(), status)
		return
	}
	defer upstream.Close()
	client, err := hijack(w)
	if err != nil {
		return
	}
	defer client.Close()
	splice(idleConn{client, tunnelIdle}, idleConn{upstream, tunnelIdle})
}

// hijack takes over the client connection and tells it the tunnel is open. Bytes the client sent
// after its CONNECT (often its TLS ClientHello) stay readable on the returned conn.
func hijack(w http.ResponseWriter) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "broker: cannot tunnel on this connection", http.StatusInternalServerError)
		return nil, errors.New("not hijackable")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		return nil, err
	}
	if rw.Reader.Buffered() > 0 {
		return &bufConn{Conn: conn, r: rw.Reader}, nil
	}
	return conn, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// idleConn pushes its deadline forward on every read and write, so a connection dies only after
// moving nothing for d.
type idleConn struct {
	net.Conn
	d time.Duration
}

func (c idleConn) Read(b []byte) (int, error) {
	c.Conn.SetDeadline(time.Now().Add(c.d))
	return c.Conn.Read(b)
}

func (c idleConn) Write(b []byte) (int, error) {
	c.Conn.SetDeadline(time.Now().Add(c.d))
	return c.Conn.Write(b)
}

// splice copies both ways until either side is done, then closes both.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}

// intercept terminates the job's TLS for host and serves the requests inside it.
func (p *Proxy) intercept(w http.ResponseWriter, host string, src netip.Addr) {
	client, err := hijack(w)
	if err != nil {
		return
	}
	tlsConn := tls.Server(idleConn{client, tunnelIdle}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != "" && normalizeHost(hello.ServerName) != host {
				return nil, fmt.Errorf("broker: TLS name %q does not match CONNECT host %q", hello.ServerName, host)
			}
			return p.CA.Leaf(host)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	err = tlsConn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		p.Log.Debug("broker: TLS handshake with job failed", "src", src, "host", host, "err", err)
		tlsConn.Close()
		return
	}
	l := newOneConnListener(tlsConn)
	srv := &http.Server{
		Handler:           p.injectHandler(host, src),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       tunnelIdle,
		ErrorLog:          slog.NewLogLogger(p.Log.Handler(), slog.LevelDebug),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				l.closeOnce()
			}
		},
	}
	srv.Serve(l)
}

// oneConnListener hands out one connection, then blocks until that connection closes.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
	mu   sync.Mutex
	used bool
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{conn: c, done: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) closeOnce()     { l.once.Do(func() { close(l.done) }) }
func (l *oneConnListener) Close() error   { l.closeOnce(); return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// injectHandler serves the requests a job sends to host inside an intercepted connection.
func (p *Proxy) injectHandler(host string, src netip.Addr) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqHost := r.Host
		if h, _, err := net.SplitHostPort(reqHost); err == nil {
			reqHost = h
		}
		if normalizeHost(reqHost) != host {
			http.Error(w, "broker: request Host does not match the CONNECT host", http.StatusMisdirectedRequest)
			return
		}
		if r.Header.Get("Upgrade") != "" {
			http.Error(w, "broker: protocol upgrades to "+host+" are not relayed", http.StatusNotImplemented)
			return
		}
		tokens, err := inject(r, host, src, func(ph, host string, src netip.Addr) (Entry, error) {
			e, err := p.Store.Lookup(ph, host, src)
			if errors.Is(err, ErrUnknownPlaceholder) && p.Resync != nil {
				p.Resync(r.Context())
				e, err = p.Store.Lookup(ph, host, src)
			}
			return e, err
		})
		if err != nil {
			p.Log.Warn("broker: refused request", "src", src, "host", host, "path", r.URL.Path, "err", err)
			http.Error(w, "broker: "+err.Error(), http.StatusForbidden)
			return
		}
		strip := newStripper(tokens)
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "https"
				pr.Out.URL.Host = host
				pr.Out.Host = host
				// Let the transport ask for gzip and decode it, so the stripper reads plain bytes.
				pr.Out.Header.Del("Accept-Encoding")
			},
			Transport:     p.upstream,
			FlushInterval: -1,
			ModifyResponse: func(resp *http.Response) error {
				if unscannable(resp) {
					return fmt.Errorf("upstream sent Content-Encoding %q, which the broker can't scan", resp.Header.Get("Content-Encoding"))
				}
				strip.header(resp.Header)
				resp.Body = strip.reader(resp.Body, resp.Trailer)
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				p.Log.Warn("broker: upstream request failed", "host", host, "path", r.URL.Path, "err", err)
				http.Error(w, "broker: upstream request failed", http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}

// forwardPlain relays a plain-HTTP proxy request to a public address. Nothing is injected, so a
// placeholder sent this way stays a placeholder, and the inject hosts are refused outright.
func (p *Proxy) forwardPlain(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "http" {
		http.Error(w, "broker: only http:// URLs can be proxied without CONNECT", http.StatusBadRequest)
		return
	}
	if isInjectHost(normalizeHost(r.URL.Hostname())) {
		http.Error(w, "broker: use https for "+r.URL.Hostname(), http.StatusForbidden)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *r.URL
			pr.Out.URL = &u
			pr.Out.Host = u.Host
		},
		Transport:     p.upstream,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, ErrNotPublic) {
				status = http.StatusForbidden
			}
			p.Log.Warn("broker: plain request failed", "host", r.URL.Host, "err", err)
			http.Error(w, "broker: "+err.Error(), status)
		},
	}
	rp.ServeHTTP(w, r)
}
