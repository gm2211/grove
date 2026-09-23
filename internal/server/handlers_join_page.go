package server

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// The join page is the answer to "how does a Mac I am standing at find out what to run".
//
// Everything else in the enrollment flow assumes you already know the control plane exists, what
// it is called, and that `grove setup` is the command. On a machine that has never met grove,
// none of that is true, and the only things to hand are a browser and whatever is on the screen
// of the machine that does know — which is exactly what a short link and a QR code are for.
//
// It sits at the root, outside /api/v1, so it is unauthenticated by construction. That is not a
// gap: a Mac joining has no credential yet (the same reason POST /join/requests is public), and
// the page's whole content is this control plane's own address and grove's published install
// commands. Reaching it at all already means being on the tailnet.
const joinPagePath = "/join"

// joinPageHTML renders with html/template, so every value below is escaped in the context it
// lands in. Deliberately one self-contained file with no asset references: it has to render on a
// machine that has loaded nothing else from this server, and it is the one page that must work
// when the SPA has not been built.
var joinPageTemplate = template.Must(template.New("join").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Join this grove</title>
<style>
  :root { color-scheme: light dark; --fg: #1b1f18; --muted: #5b6357; --bg: #f5f6f4; --card: #fff; --line: #d8dcd3; --accent: #2f6f4f; }
  @media (prefers-color-scheme: dark) {
    :root { --fg: #e8ebe3; --muted: #a3aa9b; --bg: #141712; --card: #1b1f18; --line: #2c322a; --accent: #4fae7f; }
  }
  body { margin: 0; padding: 2rem 1rem 3rem; background: var(--bg); color: var(--fg);
    font: 16px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
  main { max-width: 34rem; margin: 0 auto; }
  h1 { font-size: 1.4rem; margin: 0 0 .25rem; }
  p.lede { color: var(--muted); margin: 0 0 1.5rem; }
  section { background: var(--card); border: 1px solid var(--line); border-radius: 10px;
    padding: 1rem 1.1rem; margin-bottom: 1rem; }
  h2 { font-size: .78rem; letter-spacing: .08em; text-transform: uppercase;
    color: var(--muted); margin: 0 0 .6rem; font-weight: 600; }
  pre { margin: 0; padding: .75rem .85rem; overflow-x: auto; border-radius: 7px;
    background: var(--bg); border: 1px solid var(--line);
    font: 13px/1.6 ui-monospace, "SF Mono", Menlo, monospace; }
  .addr { font: 13px/1.6 ui-monospace, "SF Mono", Menlo, monospace; color: var(--accent);
    word-break: break-all; }
  ol { margin: .4rem 0 0; padding-left: 1.2rem; color: var(--muted); }
  li { margin: .3rem 0; }
  li strong { color: var(--fg); font-weight: 600; }
  figure { margin: 0; text-align: center; }
  figure img { width: 200px; height: 200px; background: #fff; border-radius: 8px; padding: 8px; }
  figcaption { color: var(--muted); font-size: .85rem; margin-top: .6rem; }
</style>
</head>
<body>
<main>
  <h1>Join this grove</h1>
  <p class="lede">Run these on the Mac you want in the fleet. It finds this control plane by itself.</p>

  <section>
    <h2>On the new Mac</h2>
    <pre>{{.Install}}</pre>
  </section>

  <section>
    <h2>Then</h2>
    <ol>
      <li><strong>grove setup</strong> prints an eight-character code and waits ten minutes.</li>
      <li>Someone approves it, from this grove's Fleet page or with <strong>grove join approve CODE</strong>.</li>
      <li>The Mac picks up its credentials and joins the fleet.</li>
    </ol>
  </section>

  <section>
    <h2>This control plane</h2>
    <p class="addr">{{.ServerURL}}</p>
    <figure>
      <img src="{{.QRPath}}" alt="QR code for this page's address" width="200" height="200">
      <figcaption>Scan to open this page on another device.</figcaption>
    </figure>
  </section>
</main>
</body>
</html>
`))

// groveInstall is grove's published install sequence for a worker Mac, from README.md and
// docs/INSTALL.md. grove is its own Homebrew tap, so the tap has to be added by URL first.
const groveInstall = `brew tap gm2211/grove https://github.com/gm2211/grove
brew install grove
grove setup`

// pageBase is the absolute origin to put in front of a path on this page. Options.ServerURL is
// what the control plane was told to call itself and is what the joining Mac is given elsewhere
// in the flow, so it wins; a server started without one falls back to the Host the browser
// actually used, which is by definition reachable from wherever the page is being read.
func (s *Server) pageBase(r *http.Request) string {
	if configured := strings.TrimRight(strings.TrimSpace(s.opts.ServerURL), "/"); configured != "" {
		return configured
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, r.Host)
}

func (s *Server) handleJoinPage(w http.ResponseWriter, r *http.Request) {
	base := s.pageBase(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := joinPageTemplate.Execute(w, map[string]string{
		"Install":   groveInstall,
		"ServerURL": base,
		"QRPath":    joinPagePath + "/qr.svg",
	}); err != nil {
		s.log.Error("render join page failed", "err", err)
	}
}

// handleJoinQR renders the join page's own absolute address as a QR code.
//
// SVG rather than a raster: it is a few kilobytes, stays sharp whether it is read off a laptop
// screen or a printout taped to a rack, and needs no image encoding. The quiet zone comes from
// the library's bitmap, which already includes it — cropping it would make the code unreadable
// by most scanners.
func (s *Server) handleJoinQR(w http.ResponseWriter, r *http.Request) {
	target := s.pageBase(r) + joinPagePath
	if _, err := url.Parse(target); err != nil {
		http.Error(w, "this control plane has no usable address to encode", http.StatusServiceUnavailable)
		return
	}
	code, err := qrcode.New(target, qrcode.Medium)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	bitmap := code.Bitmap()
	size := len(bitmap)
	var svg strings.Builder
	fmt.Fprintf(&svg, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code">`, size, size)
	svg.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/><path fill="#000000" d="`)
	for y, row := range bitmap {
		// One horizontal run per stretch of dark modules rather than one rect per module: the
		// same picture in a fraction of the bytes.
		for x := 0; x < size; x++ {
			if !row[x] {
				continue
			}
			run := 1
			for x+run < size && row[x+run] {
				run++
			}
			fmt.Fprintf(&svg, "M%d %dh%dv1h-%dz", x, y, run, run)
			x += run - 1
		}
	}
	svg.WriteString(`"/></svg>`)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(svg.String()))
}
