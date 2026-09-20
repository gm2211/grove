package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

var setupServer string
var setupRegistryUser string

type joinStart struct {
	ID, Secret, Code string
	ExpiresAt        time.Time
}
type joinResult struct {
	Status, ControllerURL, BootstrapToken, ServerURL string
	WorkerOnline                                     bool `json:"workerOnline"`
}

var joinHTTPClient = &http.Client{Timeout: 10 * time.Second}

func init() {
	setupCmd := &cobra.Command{
		Use: "setup", Short: "Discover and securely join this Mac to Grove.",
		Long: "Discovers an existing Grove control plane on the current Tailscale network, requests approval, installs the worker, and verifies that the control plane can see it.",
		RunE: runSetup,
	}
	setupCmd.Flags().StringVar(&setupServer, "server", "", "Grove server URL; normally discovered over Tailscale")
	setupCmd.Flags().StringVar(&setupRegistryUser, "registry-user", "", "registry username; password/PAT is read from GROVE_REGISTRY_TOKEN")
	Root.AddCommand(setupCmd)

	joinCmd := &cobra.Command{Use: "join", Short: "Approve Grove enrollment requests."}
	approve := &cobra.Command{Use: "approve CODE", Args: cobra.ExactArgs(1), Short: "Approve one pending computer", RunE: runJoinApprove}
	approve.Flags().StringVar(&setupServer, "server", "", "Grove server URL; defaults to configured server")
	joinCmd.AddCommand(approve)
	Root.AddCommand(joinCmd)
}

func runSetup(cmd *cobra.Command, _ []string) error {
	if runtimeOS() != "darwin" {
		return errors.New("`grove setup` currently joins Apple Silicon worker Macs; use `grove install --role control-plane` to create a control plane")
	}
	r := install.ExecRunner{}
	bin, err := install.FindTailscale(nil, nil)
	if err != nil {
		return fmt.Errorf("Tailscale setup required: %w", err)
	}
	st, err := install.GetTailscaleStatus(cmd.Context(), r, bin)
	if err != nil || st.BackendState != "Running" {
		return errors.New("Tailscale is not connected. Open Tailscale, sign in, then run `grove setup` again")
	}
	serverURL := strings.TrimRight(setupServer, "/")
	if serverURL == "" {
		serverURL, err = discoverGrove(cmd.Context(), st)
		if err != nil {
			return err
		}
	}
	host, _ := os.Hostname()
	start, err := createJoin(cmd.Context(), serverURL, host, st.TailnetIP())
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Found Grove at %s.\n\nApproval required. On control-plane machine, run:\n\n  grove join approve %s\n\nWaiting up to 10 minutes...\n", serverURL, start.Code)
	result, err := waitForJoin(cmd.Context(), serverURL, start)
	if err != nil {
		return err
	}
	if result.ControllerURL == "" || result.BootstrapToken == "" {
		return errors.New("control plane approved join but returned incomplete worker credentials")
	}
	opts := install.Options{
		Role: install.RoleWorker, Controller: result.ControllerURL, ServerURL: result.ServerURL,
		Token: result.BootstrapToken, RegistryUser: setupRegistryUser,
		RegistryToken: os.Getenv("GROVE_REGISTRY_TOKEN"),
	}
	if err := applyInstall(cmd, opts, false, false); err != nil {
		return err
	}
	if err := saveJoinedServer(result.ServerURL); err != nil {
		return err
	}
	if err := waitForWorker(cmd.Context(), result.ServerURL, start); err != nil {
		return fmt.Errorf("worker installed but not yet visible: %w. If macOS shows Local Network access for Grove Orchard Worker, click Allow; setup will finish after rerun", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nGrove worker %q is online and discoverable.\n", host)
	return nil
}

var runtimeOS = func() string { return runtime.GOOS }

func discoverGrove(ctx context.Context, st *install.TailscaleStatus) (string, error) {
	var ips []string
	for _, peerIPs := range st.TailnetPeers() {
		ips = append(ips, peerIPs...)
	}
	if st.TailnetIP() != "" {
		ips = append(ips, st.TailnetIP())
	}
	client := &http.Client{Timeout: 1200 * time.Millisecond}
	var found []string
	for _, ip := range ips {
		if net.ParseIP(ip) == nil || strings.Contains(ip, ":") {
			continue
		}
		base := "http://" + net.JoinHostPort(ip, "6130")
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/healthz", nil)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var body struct {
			Version string `json:"version"`
			Orchard string `json:"orchard"`
			Nomad   string `json:"nomad"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
		resp.Body.Close()
		if err == nil && (body.Orchard != "" || body.Nomad != "") {
			found = append(found, base)
		}
	}
	if len(found) == 0 {
		return "", errors.New("no Grove control plane found on this tailnet. To create the first one, run `grove install --role control-plane`; otherwise update Grove on the control plane and retry")
	}
	if len(found) > 1 {
		return "", fmt.Errorf("multiple Grove control planes found: %s; rerun with `grove setup --server URL`", strings.Join(found, ", "))
	}
	return found[0], nil
}

func createJoin(ctx context.Context, serverURL, name, ip string) (*joinStart, error) {
	body, _ := json.Marshal(map[string]string{"name": name, "tailnetIp": ip})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/api/v1/join/requests", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := joinHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Grove enrollment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return nil, errors.New("control plane needs guided-enrollment support. On that machine run `brew upgrade grove`, restart `com.grove.server`, then rerun `grove setup`")
		}
		return nil, fmt.Errorf("request Grove enrollment: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out joinStart
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func waitForJoin(ctx context.Context, serverURL string, start *joinStart) (*joinResult, error) {
	deadline := time.NewTimer(time.Until(start.ExpiresAt))
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		result, pending, err := pollJoin(ctx, serverURL, start)
		if err != nil {
			return nil, err
		}
		if !pending {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("join approval expired; run `grove setup` again")
		case <-ticker.C:
		}
	}
}

func pollJoin(ctx context.Context, serverURL string, start *joinStart) (*joinResult, bool, error) {
	return pollJoinPhase(ctx, serverURL, start, "")
}

func pollJoinPhase(ctx context.Context, serverURL string, start *joinStart, phase string) (*joinResult, bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/api/v1/join/requests/"+url.PathEscape(start.ID), nil)
	req.Header.Set("X-Grove-Join-Secret", start.Secret)
	if phase != "" {
		req.Header.Set("X-Grove-Join-Phase", phase)
	}
	resp, err := joinHTTPClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, false, fmt.Errorf("poll enrollment: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out joinResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, err
	}
	return &out, resp.StatusCode == http.StatusAccepted, nil
}

func runJoinApprove(cmd *cobra.Command, args []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configured control plane: %w", err)
	}
	base := strings.TrimRight(setupServer, "/")
	if base == "" {
		base = strings.TrimRight(cfg.Server.URL, "/")
	}
	req, _ := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/api/v1/join/approve/"+url.PathEscape(strings.ToUpper(args[0])), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Server.Token)
	resp, err := joinHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("approve join: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Join approved. New computer will finish setup automatically.")
	return nil
}

func saveJoinedServer(serverURL string) error {
	cfg, path, err := config.Load()
	if err != nil {
		if !errors.Is(err, config.ErrNotFound) {
			return err
		}
		cfg = &config.Config{}
		path, err = config.DefaultPath()
		if err != nil {
			return err
		}
	}
	cfg.Server.URL = serverURL
	return config.Save(path, cfg)
}

func waitForWorker(ctx context.Context, serverURL string, start *joinStart) error {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		result, _, err := pollJoinPhase(ctx, serverURL, start, "verify")
		if err == nil && result.WorkerOnline {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out waiting for worker registration")
		case <-ticker.C:
		}
	}
}
