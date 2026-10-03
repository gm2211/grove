package cli

import (
	"bufio"
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

var setupServer string
var setupRegistryUser string
var setupYes bool

type joinStart struct {
	ID, Secret, Code string
	ExpiresAt        time.Time
}
type joinResult struct {
	Status, ControllerURL, BootstrapToken, ServerURL, ClientToken, DeviceID string
	WorkerOnline                                                            bool `json:"workerOnline"`
}

var joinHTTPClient = &http.Client{Timeout: 10 * time.Second}

func init() {
	setupCmd := &cobra.Command{
		Use: "setup", Short: "Discover and securely join this Mac to Grove.",
		Long: "Discovers an existing Grove control plane on the current Tailscale network, requests approval, installs the worker, and verifies that the control plane can see it.",
		RunE: runSetup,
	}
	setupCmd.Flags().StringVar(&setupServer, "server", "", "Grove server URL; normally discovered over Tailscale")
	setupCmd.Flags().BoolVar(&setupYes, "yes", false, "join the discovered control plane without asking, when exactly one answers (several always need --server)")
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
		candidates := discoverGrove(cmd.Context(), st, probeGrove)
		serverURL, err = chooseGrove(candidates, cmd.InOrStdin(), cmd.OutOrStdout(), stdinIsTerminal(), setupYes)
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
	if result.ClientToken == "" || result.DeviceID == "" {
		return errors.New("control plane approved join but returned incomplete dispatcher credentials")
	}
	if err := saveJoinedServer(cmd.Context(), r, result.ServerURL, result.DeviceID, result.ClientToken); err != nil {
		return err
	}
	if err := waitForWorker(cmd.Context(), result.ServerURL, start); err != nil {
		return fmt.Errorf("worker installed but not yet visible: %w. If macOS shows Local Network access for Grove Orchard Worker, click Allow; setup will finish after rerun", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nGrove worker %q is online and discoverable. This device can view and dispatch jobs. Run `grove ui` to open Map.\n", host)
	return nil
}

var runtimeOS = func() string { return runtime.GOOS }

// groveCandidate is one tailnet peer that answered like a Grove control plane.
type groveCandidate struct {
	URL  string // http://<tailnet-ip>:6130
	Name string // the peer's MagicDNS name, for the operator to recognise
}

// probeGrove reports whether base answers /api/v1/healthz like a Grove control plane.
func probeGrove(ctx context.Context, base string) bool {
	client := &http.Client{Timeout: 1200 * time.Millisecond}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/healthz", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		Version string `json:"version"`
		Orchard string `json:"orchard"`
		Nomad   string `json:"nomad"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	return err == nil && (body.Orchard != "" || body.Nomad != "")
}

// discoverGrove probes every online tailnet peer (and this machine) on :6130 and returns all that
// answer like Grove, sorted for a stable listing. It never picks one: see chooseGrove.
func discoverGrove(ctx context.Context, st *install.TailscaleStatus, probe func(context.Context, string) bool) []groveCandidate {
	type target struct{ ip, name string }
	var targets []target
	for name, peerIPs := range st.TailnetPeers() {
		for _, ip := range peerIPs {
			targets = append(targets, target{ip, name})
		}
	}
	if st.TailnetIP() != "" {
		targets = append(targets, target{st.TailnetIP(), st.Self.DNSName + " (this machine)"})
	}
	var found []groveCandidate
	for _, t := range targets {
		if net.ParseIP(t.ip) == nil || strings.Contains(t.ip, ":") {
			continue
		}
		base := "http://" + net.JoinHostPort(t.ip, "6130")
		if probe(ctx, base) {
			found = append(found, groveCandidate{URL: base, Name: strings.TrimSuffix(t.name, ".")})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].URL < found[j].URL })
	return found
}

var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// chooseGrove turns discovery results into the server setup will enrol with. A discovered peer is
// only a tailnet node answering plain HTTP on :6130, so setup never silently trusts one: a single
// candidate is shown and needs confirmation (or --yes); several are listed and the operator picks
// one (or passes --server). Without a terminal and without --yes/--server it refuses.
func chooseGrove(found []groveCandidate, in io.Reader, out io.Writer, interactive, yes bool) (string, error) {
	switch len(found) {
	case 0:
		return "", errors.New("no Grove control plane found on this tailnet. To create the first one, run `grove install --role control-plane`; otherwise update Grove on the control plane and retry")
	case 1:
		c := found[0]
		fmt.Fprintf(out, "Discovered a Grove control plane on this tailnet:\n\n  %s  (%s)\n\n", c.URL, c.Name)
		if yes {
			return c.URL, nil
		}
		if !interactive {
			return "", fmt.Errorf("refusing to enrol with a discovered control plane unattended; confirm it is yours, then rerun with `--yes` or `--server %s`", c.URL)
		}
		fmt.Fprint(out, "Is this your control plane? This Mac will send it an enrollment request and later run its jobs. [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return c.URL, nil
		}
		return "", errors.New("setup cancelled; rerun with `grove setup --server URL` to pick a control plane explicitly")
	}
	var list strings.Builder
	for i, c := range found {
		fmt.Fprintf(&list, "  %d) %s  (%s)\n", i+1, c.URL, c.Name)
	}
	fmt.Fprintf(out, "Several tailnet peers answer like a Grove control plane:\n\n%s\n", list.String())
	if !interactive {
		return "", fmt.Errorf("multiple Grove control planes found; rerun with `grove setup --server URL` naming the one to join")
	}
	fmt.Fprintf(out, "Which one should this Mac join? [1-%d, anything else cancels] ", len(found))
	answer, _ := bufio.NewReader(in).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(found) {
		return "", errors.New("setup cancelled; rerun with `grove setup --server URL` to pick a control plane explicitly")
	}
	return found[n-1].URL, nil
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
	token, err := config.ResolveOperatorToken(cfg)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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

func saveJoinedServer(ctx context.Context, r install.Runner, serverURL, deviceID, token string) error {
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
	cfg.Server.TokenKeychain = deviceID
	if err := install.StoreKeychainSecret(ctx, r, deviceID, config.ServerTokenKeychainService, token); err != nil {
		return fmt.Errorf("store dispatcher credential in Keychain: %w", err)
	}
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
