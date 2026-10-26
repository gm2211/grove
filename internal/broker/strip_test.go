package broker

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const testToken = "sk-ant-oat01-Abc/def+ghi=jkl_mno~pqr"

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStripperMasksEveryForm(t *testing.T) {
	s := newStripper([]string{testToken})
	forms := map[string]string{
		"raw":         testToken,
		"query":       url.QueryEscape(testToken),
		"path":        url.PathEscape(testToken),
		"json":        strings.ReplaceAll(testToken, "/", `\/`),
		"json-escape": `"` + testToken + `"`,
	}
	for k := 0; k < 3; k++ {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding} {
			b := append([]byte(strings.Repeat("p", k)), testToken...)
			b = append(b, "trailing bytes"...)
			forms["base64-"+string(rune('0'+k))+"-"+enc.EncodeToString([]byte{0xfb})] = enc.EncodeToString(b)
		}
	}
	for name, form := range forms {
		body := "before " + form + " after"
		// One byte at a time, so every pattern straddles reads.
		got := readAll(t, s.reader(io.NopCloser(iotest.OneByteReader(strings.NewReader(body))), nil))
		if len(got) != len(body) {
			t.Fatalf("%s: length changed %d -> %d", name, len(body), len(got))
		}
		if strings.Contains(got, testToken) || got == body {
			t.Fatalf("%s: not masked: %q", name, got)
		}
		if name == "raw" && got != "before "+strings.Repeat("x", len(testToken))+" after" {
			t.Fatalf("raw: %q", got)
		}
		if strings.HasPrefix(name, "base64") {
			dec, _ := base64.StdEncoding.DecodeString(got[len("before ") : len(got)-len(" after")])
			if bytes.Contains(dec, []byte(testToken)) {
				t.Fatalf("%s: decodes back to the token", name)
			}
		}
	}
}

func TestStripperLeavesOtherBytesAlone(t *testing.T) {
	s := newStripper([]string{testToken})
	body := strings.Repeat("hello world, nothing secret here\n", 5000)
	if got := readAll(t, s.reader(io.NopCloser(strings.NewReader(body)), nil)); got != body {
		t.Fatal("clean body changed")
	}
	if r := newStripper(nil).reader(io.NopCloser(strings.NewReader(body)), nil); readAll(t, r) != body {
		t.Fatal("no-token stripper changed the body")
	}
}

func TestStripperHeadersAndTrailers(t *testing.T) {
	s := newStripper([]string{testToken})
	h := http.Header{"X-Echo": {"Bearer " + testToken}, "Content-Type": {"application/json"}}
	s.header(h)
	if strings.Contains(h.Get("X-Echo"), testToken) || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", h)
	}
	trailer := http.Header{}
	body := &trailerBody{r: strings.NewReader("data"), trailer: trailer, value: testToken}
	readAll(t, s.reader(body, trailer))
	if strings.Contains(trailer.Get("X-Trailer"), testToken) {
		t.Fatalf("trailer not masked: %v", trailer)
	}
}

// trailerBody fills its trailer at EOF, as the transport does.
type trailerBody struct {
	r       io.Reader
	trailer http.Header
	value   string
}

func (b *trailerBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.trailer.Set("X-Trailer", b.value)
	}
	return n, err
}
func (b *trailerBody) Close() error { return nil }

// An event must reach the job as soon as its terminating newline does, not when the stream ends.
func TestStripperReleasesAtNewline(t *testing.T) {
	s := newStripper([]string{testToken})
	pr, pw := io.Pipe()
	r := s.reader(pr, nil)
	go pw.Write([]byte("event: ping\ndata: {}\n\n"))
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := io.ReadAtLeast(r, buf, len("event: ping\ndata: {}\n\n"))
		got <- string(buf[:n])
	}()
	select {
	case g := <-got:
		if g != "event: ping\ndata: {}\n\n" {
			t.Fatalf("got %q", g)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event held back until more data arrived")
	}
	pw.Close()
}

func TestUnscannable(t *testing.T) {
	for ce, want := range map[string]bool{"": false, "identity": false, "br": true, "gzip": true} {
		resp := &http.Response{Header: http.Header{}}
		if ce != "" {
			resp.Header.Set("Content-Encoding", ce)
		}
		if got := unscannable(resp); got != want {
			t.Fatalf("Content-Encoding %q: %t, want %t", ce, got, want)
		}
	}
}
