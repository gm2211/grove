package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/config"
)

func init() {
	cmd := &cobra.Command{
		Use: "ui", Short: "Open Grove UI using this device's Keychain credential.",
		RunE: func(cmd *cobra.Command, _ []string) error { return runUI(cmd) },
	}
	Root.AddCommand(cmd)
}

func runUI(cmd *cobra.Command) error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Server.TokenKeychain == "" {
		return fmt.Errorf("no device-scoped UI credential configured; run `grove setup` (operator credentials are never forwarded to a browser)")
	}
	remote, err := url.Parse(cfg.Server.URL)
	if err != nil || remote.Scheme == "" || remote.Host == "" {
		return fmt.Errorf("invalid server.url %q", cfg.Server.URL)
	}
	token, err := config.ResolveServerToken(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("no Grove device credential configured; run `grove setup`")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return err
	}
	session := hex.EncodeToString(secretBytes)
	proxy := httputil.NewSingleHostReverseProxy(remote)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, cookieErr := r.Cookie("grove_ui")
		if cookieErr != nil && r.Method == http.MethodGet && r.URL.Path == "/session/"+session {
			http.SetCookie(w, &http.Cookie{Name: "grove_ui", Value: session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if cookieErr != nil || cookie.Value != session {
			http.Error(w, "unauthorized local Grove UI session", http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	localURL := "http://" + listener.Addr().String() + "/"
	fmt.Fprintf(cmd.OutOrStdout(), "Grove UI: %s\n", localURL)
	bootstrapURL := localURL + "session/" + session
	openScript := "open location " + fmt.Sprintf("%q", bootstrapURL) + "\n"
	openCmd := exec.CommandContext(cmd.Context(), "/usr/bin/osascript", "-")
	openCmd.Stdin = strings.NewReader(openScript)
	if err := openCmd.Run(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	go func() {
		<-cmd.Context().Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
