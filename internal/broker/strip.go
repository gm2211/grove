package broker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// stripper masks real tokens in a response. Upstreams have no reason to send a token back, but a
// job is treated as hostile, so anything that looks like one of the tokens injected into the
// request is overwritten before the job sees it: the token as is, URL-escaped, JSON-escaped, and
// base64 (both alphabets, every byte alignment, so it is caught inside a longer encoded blob).
//
// Every pattern is replaced by a mask of the same length, so Content-Length stays right and the
// body can be rewritten as it streams.
type stripper struct {
	patterns [][]byte
	longest  int
}

// minBase64Token is the shortest token whose base64 forms are masked too. A shorter token's
// encoded core is short enough to match ordinary encoded data by chance.
const minBase64Token = 16

func newStripper(tokens []string) *stripper {
	s := &stripper{}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		s.patterns = append(s.patterns, []byte(p))
		if len(p) > s.longest {
			s.longest = len(p)
		}
	}
	for _, t := range tokens {
		add(t)
		add(url.QueryEscape(t))
		add(url.PathEscape(t))
		if j, err := json.Marshal(t); err == nil {
			add(string(j[1 : len(j)-1]))
		}
		add(strings.ReplaceAll(t, "/", `\/`)) // JSON encoders that escape the slash
		if len(t) >= minBase64Token {
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
				for k := 0; k < 3; k++ {
					add(base64Core(enc, t, k))
				}
			}
		}
	}
	return s
}

// base64Core returns the part of t's base64 encoding that doesn't depend on what surrounds t, when
// t starts k bytes into a 3-byte group: the groups made of t's bytes only.
func base64Core(enc *base64.Encoding, t string, k int) string {
	b := append(make([]byte, k), t...)
	full := len(b) / 3 * 3
	out := enc.WithPadding(base64.NoPadding).EncodeToString(b[:full])
	if k > 0 {
		out = out[4:]
	}
	return out
}

func (s *stripper) empty() bool { return len(s.patterns) == 0 }

// mask overwrites every pattern in b, in place.
func (s *stripper) mask(b []byte) {
	for _, p := range s.patterns {
		for i := 0; ; {
			j := bytes.Index(b[i:], p)
			if j < 0 {
				break
			}
			for k := range p {
				b[i+j+k] = 'x'
			}
			i += j + len(p)
		}
	}
}

func (s *stripper) maskString(v string) string {
	b := []byte(v)
	s.mask(b)
	return string(b)
}

// header masks every value in h.
func (s *stripper) header(h http.Header) {
	for name, values := range h {
		for i, v := range values {
			values[i] = s.maskString(v)
		}
		h[name] = values
	}
}

// reader wraps body so every pattern is masked as it streams. It holds back only the bytes a
// pattern could still be completing: none past a newline, since no pattern spans one. A
// server-sent event therefore goes out whole as soon as its blank line arrives. trailer, the
// response's Trailer map, is masked once the body ends, which is when the transport fills it in.
func (s *stripper) reader(body io.ReadCloser, trailer http.Header) io.ReadCloser {
	if s.empty() {
		return body
	}
	return &stripReader{s: s, src: body, trailer: trailer}
}

type stripReader struct {
	s       *stripper
	src     io.ReadCloser
	trailer http.Header
	buf     []byte // read, masked, not yet released
	out     []byte // released to the caller
	err     error
	tmp     [32 << 10]byte
}

func (r *stripReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		n, err := r.src.Read(r.tmp[:])
		r.buf = append(r.buf, r.tmp[:n]...)
		r.s.mask(r.buf)
		if err != nil {
			r.out, r.buf, r.err = r.buf, nil, err
			r.s.header(r.trailer)
			break
		}
		safe := len(r.buf) - (r.s.longest - 1)
		if nl := bytes.LastIndexByte(r.buf, '\n'); nl+1 > safe {
			safe = nl + 1
		}
		if safe > len(r.buf) {
			safe = len(r.buf)
		}
		if safe > 0 {
			r.out = append(r.out[:0], r.buf[:safe]...)
			r.buf = append(r.buf[:0], r.buf[safe:]...)
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *stripReader) Close() error { return r.src.Close() }

// unscannable reports whether a response body is still encoded in a way the stripper can't read
// through. The proxy asks upstreams for gzip only, which Go's transport decodes itself, so this is
// an upstream sending an encoding nobody asked for.
func unscannable(resp *http.Response) bool {
	ce := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	return ce != "" && !strings.EqualFold(ce, "identity")
}
