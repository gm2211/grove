package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

func getPage(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestJoinPageIsReadableWithoutACredential(t *testing.T) {
	// A token IS configured: a Mac that has never met this grove still has to be able to read
	// the page, because reading it is how it learns what to run to get a credential at all.
	srv := newTestServer(nil, nil, nil, Options{Token: "operator-secret", ServerURL: "http://100.64.0.1:6130"})

	page := getPage(t, srv, joinPagePath)
	if page.Code != http.StatusOK {
		t.Fatalf("join page status=%d, want 200", page.Code)
	}
	if ct := page.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("join page content-type=%q", ct)
	}
	body := page.Body.String()
	for _, want := range []string{
		"brew tap gm2211/grove https://github.com/gm2211/grove",
		"grove setup",
		"http://100.64.0.1:6130",
		joinPagePath + "/qr.svg",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("join page is missing %q", want)
		}
	}

	qr := getPage(t, srv, joinPagePath+"/qr.svg")
	if qr.Code != http.StatusOK {
		t.Fatalf("qr status=%d, want 200", qr.Code)
	}
	if ct := qr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("qr content-type=%q", ct)
	}
}

// The SVG is hand-rolled from the library's bitmap, so the thing worth asserting is that the
// transcription is exact: every dark module and no others. A QR that is one run off does not
// degrade, it simply does not scan, and nothing else in the build would notice.
func TestJoinQRDrawsExactlyTheEncodedBitmap(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{ServerURL: "http://100.64.0.1:6130"})
	svg := getPage(t, srv, joinPagePath+"/qr.svg").Body.String()

	expected, err := qrcode.New("http://100.64.0.1:6130"+joinPagePath, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	bitmap := expected.Bitmap()
	size := len(bitmap)

	if !strings.Contains(svg, "viewBox=\"0 0 "+strconv.Itoa(size)+" "+strconv.Itoa(size)+"\"") {
		t.Fatalf("svg viewBox does not match the %dx%d bitmap: %.120s", size, size, svg)
	}

	drawn := make([][]bool, size)
	for i := range drawn {
		drawn[i] = make([]bool, size)
	}
	runs := regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-\d+z`).FindAllStringSubmatch(svg, -1)
	if len(runs) == 0 {
		t.Fatalf("svg drew no modules at all: %.200s", svg)
	}
	for _, run := range runs {
		x, _ := strconv.Atoi(run[1])
		y, _ := strconv.Atoi(run[2])
		width, _ := strconv.Atoi(run[3])
		for i := 0; i < width; i++ {
			if y >= size || x+i >= size {
				t.Fatalf("svg drew a module outside the bitmap at %d,%d", x+i, y)
			}
			drawn[y][x+i] = true
		}
	}
	for y := range bitmap {
		for x := range bitmap[y] {
			if drawn[y][x] != bitmap[y][x] {
				t.Fatalf("module %d,%d: drawn=%v, encoded=%v", x, y, drawn[y][x], bitmap[y][x])
			}
		}
	}
}

// A control plane started without a --server-url still has to hand out an address that works,
// and the only one it can be sure of is the one the reader already reached it on.
func TestJoinPageFallsBackToTheHostTheReaderUsed(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, joinPagePath, nil)
	req.Host = "grove.tailnet.ts.net:6130"
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "http://grove.tailnet.ts.net:6130") {
		t.Fatalf("join page did not fall back to the request host: %.400s", rec.Body.String())
	}
}
