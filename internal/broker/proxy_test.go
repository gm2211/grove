package broker

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const realGitHub = "ghs_REALGITHUBTOKEN0123456789abcdef"

// harness runs a Proxy on 127.0.0.1 in front of fake upstreams. The proxy's Dialer is the real
// one, with name lookups and dials redirected to the fakes, so the public-only check is exercised.
type harness struct {
	t       *testing.T
	proxy   *Proxy
	addr    string
	store   *Store
	ca      *CA
	gh      Entry
	sawAuth chan string
	release chan struct{}
	echoTCP string
}

func newHarness(t *testing.T, configure ...func(*harness)) *harness {
	t.Helper()
	h := &harness{t: t, sawAuth: make(chan string, 16), release: make(chan struct{})}

	tlsUp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		select {
		case h.sawAuth <- auth:
		default:
		}
		switch r.URL.Path {
		case "/echo":
			w.Header().Set("X-Echo", auth)
			fmt.Fprintf(w, "you sent %s and %s", auth, base64.StdEncoding.EncodeToString([]byte(auth)))
		case "/gzip":
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				t.Errorf("proxy did not ask the upstream for gzip")
			}
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			fmt.Fprintf(zw, "you sent %s", auth)
			zw.Close()
		case "/br":
			w.Header().Set("Content-Encoding", "br")
			fmt.Fprint(w, auth)
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: one\n\n")
			w.(http.Flusher).Flush()
			<-h.release
			fmt.Fprint(w, "data: two\n\n")
		default:
			fmt.Fprint(w, "ok")
		}
	}))
	t.Cleanup(tlsUp.Close)
	plainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "plain saw %q", r.Header.Get("Authorization"))
	}))
	t.Cleanup(plainUp.Close)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	routes := map[string]string{
		"140.82.112.6:443":  tlsUp.Listener.Addr().String(),
		"93.184.216.34:80":  plainUp.Listener.Addr().String(),
		"93.184.216.34:443": echo.Addr().String(),
	}
	dialer := &Dialer{
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "api.github.com", "github.com", "api.anthropic.com":
				return []netip.Addr{netip.MustParseAddr("140.82.112.6")}, nil
			case "public.test":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			case "private.test":
				return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
			}
			return nil, fmt.Errorf("no such host %s", host)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			to, ok := routes[addr]
			if !ok {
				return nil, fmt.Errorf("test: no route to %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, to)
		},
	}
	upTLS := tlsUp.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	upTLS.ServerName = "example.com"

	h.ca, err = LoadOrCreateCA("")
	if err != nil {
		t.Fatal(err)
	}
	h.store = NewStore(0)
	h.gh = newEntry(t, "job-1", KindGitHub, realGitHub)
	if err := h.store.Replace([]Entry{h.gh}); err != nil {
		t.Fatal(err)
	}
	h.proxy = &Proxy{
		Store:  h.store,
		CA:     h.ca,
		Dialer: dialer,
		Upstream: &http.Transport{
			DialContext:     dialer.DialContext,
			TLSClientConfig: upTLS,
		},
	}
	for _, c := range configure {
		c(h)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.proxy.Serve(ctx, l)
	h.addr = l.Addr().String()
	h.echoTCP = echo.Addr().String()
	return h
}

// client is what a job VM looks like: HTTPS_PROXY pointing at the broker, the broker CA trusted.
func (h *harness) client() *http.Client {
	u, _ := url.Parse("http://" + h.addr)
	return &http.Client{Transport: &http.Transport{
		Proxy:              http.ProxyURL(u),
		TLSClientConfig:    &tls.Config{RootCAs: caPool(h.t, h.ca)},
		DisableCompression: true,
	}}
}

func (h *harness) get(rawURL, auth string) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (h *harness) upstreamSaw() string {
	select {
	case a := <-h.sawAuth:
		return a
	default:
		return "<upstream not called>"
	}
}

func TestProxyInjectsAndMasks(t *testing.T) {
	h := newHarness(t)
	resp, body := h.get("https://api.github.com/echo", "Bearer "+h.gh.Placeholder)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := h.upstreamSaw(); got != "Bearer "+realGitHub {
		t.Fatalf("upstream saw %q, want the real token", got)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("Bearer " + realGitHub))
	for _, leak := range []string{realGitHub, b64[8:40]} {
		if strings.Contains(body, leak) || strings.Contains(resp.Header.Get("X-Echo"), leak) {
			t.Fatalf("response leaks the token: header %q body %q", resp.Header.Get("X-Echo"), body)
		}
	}
	if !strings.HasPrefix(body, "you sent Bearer xxxx") {
		t.Fatalf("body = %q", body)
	}

	// gzip from the upstream is decoded and scanned too.
	resp, body = h.get("https://api.github.com/gzip", "Bearer "+h.gh.Placeholder)
	if resp.StatusCode != 200 || strings.Contains(body, realGitHub) || !strings.HasPrefix(body, "you sent Bearer xxxx") {
		t.Fatalf("gzip: %d %q", resp.StatusCode, body)
	}
	h.upstreamSaw()

	// An encoding the broker can't read through is refused, not passed on unscanned.
	resp, body = h.get("https://api.github.com/br", "Bearer "+h.gh.Placeholder)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, realGitHub) {
		t.Fatalf("br: %d %q", resp.StatusCode, body)
	}
}

func TestProxyRefusals(t *testing.T) {
	var resyncs int
	var mu sync.Mutex
	h := newHarness(t, func(h *harness) {
		h.proxy.Resync = func(context.Context) { mu.Lock(); resyncs++; mu.Unlock() }
	})
	unknown := "gph1_" + strings.Repeat("z", 52)

	bound := newEntry(t, "job-2", KindGitHub, "ghs_BOUNDELSEWHERE")
	bound.Source = netip.MustParseAddr("192.168.64.9")
	ant := newEntry(t, "job-1", KindAnthropic, "sk-ant-REAL")
	if err := h.store.Replace([]Entry{h.gh, bound, ant}); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		url, auth string
		status    int
	}{
		"unknown placeholder":         {"https://api.github.com/echo", "Bearer " + unknown, http.StatusForbidden},
		"another VM's placeholder":    {"https://api.github.com/echo", "Bearer " + bound.Placeholder, http.StatusForbidden},
		"anthropic token to github":   {"https://api.github.com/echo", "Bearer " + ant.Placeholder, http.StatusForbidden},
		"plain http to inject host":   {"http://api.github.com/", "Bearer " + h.gh.Placeholder, http.StatusForbidden},
		"plain http to private range": {"http://private.test/", "", http.StatusForbidden},
	} {
		resp, body := h.get(tc.url, tc.auth)
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: %d %q, want %d", name, resp.StatusCode, body, tc.status)
		}
		if got := h.upstreamSaw(); got != "<upstream not called>" {
			t.Fatalf("%s: upstream was called with %q", name, got)
		}
		if strings.Contains(body, "REAL") || strings.Contains(body, "BOUND") {
			t.Fatalf("%s: refusal leaks a token: %q", name, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if resyncs != 1 {
		t.Fatalf("resyncs = %d, want 1 (for the unknown placeholder only)", resyncs)
	}
}

func TestProxyResyncFindsNewEntry(t *testing.T) {
	fresh := newEntry(t, "job-new", KindGitHub, "ghs_FRESH")
	h := newHarness(t, func(h *harness) {
		h.proxy.Resync = func(context.Context) { h.store.Replace([]Entry{h.gh, fresh}) }
	})
	resp, _ := h.get("https://api.github.com/", "Bearer "+fresh.Placeholder)
	if resp.StatusCode != 200 || h.upstreamSaw() != "Bearer ghs_FRESH" {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// rawConnect opens a CONNECT tunnel by hand, for the cases an http.Client won't produce.
func rawConnect(t *testing.T, proxyAddr, target string) (net.Conn, int) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return &bufConn{Conn: c, r: br}, resp.StatusCode
}

func TestProxyInterceptChecksNames(t *testing.T) {
	h := newHarness(t)
	pool := caPool(t, h.ca)

	// TLS name differs from the CONNECT host.
	c, code := rawConnect(t, h.addr, "api.github.com:443")
	if code != 200 {
		t.Fatalf("CONNECT = %d", code)
	}
	if err := tls.Client(c, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool}).Handshake(); err == nil {
		t.Fatal("handshake for api.anthropic.com inside a CONNECT to api.github.com succeeded")
	}
	c.Close()

	// Host header differs from the CONNECT host.
	c, _ = rawConnect(t, h.addr, "api.github.com:443")
	tc := tls.Client(c, &tls.Config{ServerName: "api.github.com", RootCAs: pool})
	fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer %s\r\n\r\n", h.gh.Placeholder)
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("mismatched Host: %d, want 421", resp.StatusCode)
	}
	c.Close()

	// No protocol upgrades on an intercepted host.
	c, _ = rawConnect(t, h.addr, "api.github.com:443")
	tc = tls.Client(c, &tls.Config{ServerName: "api.github.com", RootCAs: pool})
	fmt.Fprint(tc, "GET / HTTP/1.1\r\nHost: api.github.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	resp, err = http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("upgrade: %d, want 501", resp.StatusCode)
	}
	c.Close()
	if got := h.upstreamSaw(); got != "<upstream not called>" {
		t.Fatalf("upstream was called with %q", got)
	}
}

func TestProxyStreamsEvents(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/sse", nil)
	req.Header.Set("Authorization", "Bearer "+h.gh.Placeholder)
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if line != "data: one\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event did not arrive while the stream stayed open")
	}
	close(h.release)
}

func TestProxyTunnelsOnlyToPublicAddresses(t *testing.T) {
	h := newHarness(t)
	c, code := rawConnect(t, h.addr, "public.test:443")
	if code != 200 {
		t.Fatalf("CONNECT public.test = %d", code)
	}
	fmt.Fprint(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel echo = %q, %v", buf, err)
	}
	c.Close()

	for _, target := range []string{"private.test:443", h.echoTCP, "192.168.64.1:22", "100.100.100.100:80", "[::1]:443"} {
		c, code := rawConnect(t, h.addr, target)
		c.Close()
		if code != http.StatusForbidden {
			t.Fatalf("CONNECT %s = %d, want 403", target, code)
		}
	}

	// Plain HTTP to a public host is relayed, and a placeholder is not swapped there.
	resp, body := h.get("http://public.test/", "Bearer "+h.gh.Placeholder)
	if resp.StatusCode != 200 || body != fmt.Sprintf("plain saw %q", "Bearer "+h.gh.Placeholder) {
		t.Fatalf("plain: %d %q", resp.StatusCode, body)
	}
}

func TestProxyLocalEndpoints(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.proxy.Synced = func() bool { return true } })
	resp, err := http.Get("http://" + h.addr + "/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != string(h.ca.CertPEM()) {
		t.Fatal("/ca.pem is not the CA")
	}
	resp, err = http.Get("http://" + h.addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		OK      bool `json:"ok"`
		Entries int  `json:"entries"`
		Synced  bool `json:"synced"`
	}
	json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if !health.OK || health.Entries != 1 || !health.Synced {
		t.Fatalf("healthz = %+v", health)
	}
}

func TestProxyCapsConnectionsPerSource(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.proxy.MaxConnsPerSource = 1 })
	held, code := rawConnect(t, h.addr, "public.test:443")
	if code != 200 {
		t.Fatalf("first CONNECT = %d", code)
	}
	defer held.Close()
	c, code := rawConnect(t, h.addr, "public.test:443")
	c.Close()
	if code != http.StatusTooManyRequests {
		t.Fatalf("second CONNECT = %d, want 429", code)
	}
}
