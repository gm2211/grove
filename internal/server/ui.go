// Package server hosts grove's HTTP API and its embedded web UI.
//
// This file owns only the UI half: UIHandler serves the Vite-built SPA (or, on a fresh clone
// where `make ui` hasn't run yet, a small placeholder page) with client-side-routing fallback.
// The API half (/api/v1/...) is owned elsewhere in this package; it mounts UIHandler() at "/".
package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// distFS embeds ui/'s build output. vite.config.ts sets build.outDir to ../internal/server/dist
// (relative to ui/) because go:embed cannot reach outside this module's directory tree. The
// "all:" prefix is required so the placeholder dist/.keep file (a dotfile, normally excluded by
// go:embed) is still embedded — without it `go build` fails on a fresh clone with "no matching
// files found" whenever dist/ is otherwise empty.
//
//go:embed all:dist
var distFS embed.FS

const uiNotBuiltPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>grove</title></head>
<body style="font-family: -apple-system, sans-serif; padding: 3rem; max-width: 40rem; margin: 0 auto;">
<h1>grove UI not built</h1>
<p>This server binary was built without the web UI bundled in. Run <code>make ui</code> (or
<code>cd ui &amp;&amp; npm ci &amp;&amp; npm run build</code>) from the repo root, then rebuild
<code>grove</code>.</p>
<p>The API itself is unaffected — <code>/api/v1/healthz</code> and friends work normally.</p>
</body>
</html>
`

// UIHandler serves the embedded SPA at "/". Unknown non-file paths fall back to index.html so
// client-side routing (react-router) works on a hard refresh of e.g. /jobs/abc123. Hashed assets
// under /assets/ get a long, immutable cache lifetime; index.html itself is never cached so a new
// deploy is picked up on the next load.
func UIHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Only fails if the "dist" directory is missing from the embed, which can't happen given
		// the go:embed directive above always embeds it (even if empty save for .keep).
		panic(err)
	}
	root := sub

	built := distHasUI(root)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(uiNotBuiltPage))
			return
		}

		upath := r.URL.Path
		if !strings.HasPrefix(upath, "/") {
			upath = "/" + upath
		}
		cleaned := path.Clean(strings.TrimPrefix(upath, "/"))

		serveName := cleaned
		if f, err := fs.Stat(root, cleaned); err != nil || f.IsDir() {
			// No matching file (or it's a directory): this is an SPA route, serve index.html.
			serveName = "index.html"
		}

		if strings.HasPrefix(cleaned, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		http.ServeFileFS(w, r, root, serveName)
	})
}

// distHasUI reports whether a real build (not just the placeholder .keep) has been embedded.
func distHasUI(root fs.FS) bool {
	_, err := fs.Stat(root, "index.html")
	return err == nil
}
