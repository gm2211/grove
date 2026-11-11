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
	"path/filepath"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

// brokerJoiner gets worker its broker credential from the control plane and returns the token
// and the credential's device id; selfJoinBroker in production.
type brokerJoiner func(ctx context.Context, worker string) (token, deviceID string, err error)

// controlPlaneBrokerToken gives the worker on the control-plane host its broker credential. A
// worker anywhere else gets one from join approval (`grove setup`), but the control plane's own
// worker is installed with `grove install --role worker` and never joins Orchard that way, so its
// broker ran with no credential and could never sync (doctor: "no broker credential file"). A
// broker credential is issued only by join approval, bound to the machine that asked, so this
// host joins for that credential alone (selfJoinBroker) and approves its own request with the
// operator token it already holds: scope broker only, named after the Orchard worker, bound to
// the address this host reaches its own `grove serve` from, exactly what GET /broker/entries
// demands.
//
// It returns "" (and the install goes on without one, as before) when this isn't the control
// plane, a credential is already stored, or the control plane can't issue one; the last says so.
func controlPlaneBrokerToken(ctx context.Context, cfg *config.Config, credential, worker string, join brokerJoiner, warn io.Writer) string {
	if cfg == nil || cfg.Server.Listen == "" || cfg.Server.URL == "" || worker == "" {
		return ""
	}
	if _, err := os.Lstat(credential); !errors.Is(err, os.ErrNotExist) {
		return "" // stored already (or unreadable: the broker-credential step and doctor report that)
	}
	token, deviceID, err := join(ctx, worker)
	if err != nil || token == "" {
		fmt.Fprintf(warn, "warning: could not issue this control plane's own broker credential (%v), so jobs on its worker can't use credentials yet. Start `grove serve`, then run this install again.\n", err)
		return ""
	}
	fmt.Fprintf(warn, "Issued this control plane's worker %q its broker credential (device %s; revoke with `grove access revoke %s`).\n", worker, deviceID, deviceID)
	return token
}

// withControlPlaneBrokerToken fills opts.BrokerToken for a worker install on the control-plane
// host (see controlPlaneBrokerToken). Never on a dry run: issuing is a change.
func withControlPlaneBrokerToken(ctx context.Context, opts install.Options, dryRun bool, warn io.Writer) install.Options {
	if opts.Role != install.RoleWorker || opts.BrokerToken != "" || dryRun {
		return opts
	}
	cfg, _, err := config.Load()
	if err != nil {
		return opts
	}
	operator, err := config.ResolveOperatorToken(cfg)
	if err != nil {
		return opts
	}
	dir, err := config.CredentialsDir()
	if err != nil {
		return opts
	}
	worker := opts.Hostname
	if worker == "" {
		worker = install.InstalledWorkerName(opts)
	}
	if worker == "" {
		worker, _ = os.Hostname()
	}
	opts.BrokerToken = controlPlaneBrokerToken(ctx, cfg, filepath.Join(dir, config.BrokerCredentialFile), worker, selfJoinBroker(cfg.Server.URL, operator), warn)
	return opts
}

// selfJoinBroker joins the control plane at serverURL for a broker credential alone, from the
// control plane's own host, approving the request with the operator token:
//
//  1. POST /join/requests {name, tailnetIp, brokerOnly} from the address this host reaches its
//     own `grove serve` from (the control plane binds the credential to it; the broker daemon
//     uses the same server URL, so the same address);
//  2. POST /join/approve/<code>?replace=true: this worker usually exists in Orchard already and,
//     since no credential file is stored here, whatever broker credential it had is lost;
//  3. poll for the token, then tell the control plane it arrived, which retires the older ones.
//
// A control plane that predates broker-only joins would run a full join (a new Orchard account
// for a worker that has one), so the request is approved only if the answer echoes brokerOnly.
func selfJoinBroker(serverURL, operatorToken string) brokerJoiner {
	return func(ctx context.Context, worker string) (string, string, error) {
		base := strings.TrimRight(serverURL, "/")
		source, err := localAddrToward(ctx, base)
		if err != nil {
			return "", "", err
		}
		body, _ := json.Marshal(map[string]any{"name": worker, "tailnetIp": source, "brokerOnly": true})
		var start struct {
			joinStart
			BrokerOnly bool `json:"brokerOnly"`
		}
		if err := joinCall(ctx, http.MethodPost, base+"/api/v1/join/requests", nil, body, http.StatusCreated, &start); err != nil {
			return "", "", fmt.Errorf("request the broker credential: %w", err)
		}
		if !start.BrokerOnly {
			return "", "", errors.New("the running `grove serve` predates broker-only joins: restart it (it is still the old version), then run this install again")
		}
		auth := map[string]string{"Authorization": "Bearer " + operatorToken}
		if err := joinCall(ctx, http.MethodPost, base+"/api/v1/join/approve/"+url.PathEscape(start.Code)+"?replace=true", auth, nil, http.StatusOK, nil); err != nil {
			return "", "", fmt.Errorf("approve the broker credential: %w", err)
		}
		secret := map[string]string{"X-Grove-Join-Secret": start.Secret}
		var result struct {
			BrokerToken    string `json:"brokerToken"`
			BrokerDeviceID string `json:"brokerDeviceId"`
		}
		poll := base + "/api/v1/join/requests/" + url.PathEscape(start.ID)
		if err := joinCall(ctx, http.MethodGet, poll, secret, nil, http.StatusOK, &result); err != nil {
			return "", "", fmt.Errorf("collect the broker credential: %w", err)
		}
		if result.BrokerToken == "" {
			return "", "", errors.New("the control plane approved the request but sent no broker credential")
		}
		// Best effort: unacknowledged, the request just expires and the older credentials stay.
		secret["X-Grove-Join-Phase"] = "verify"
		_ = joinCall(ctx, http.MethodGet, poll, secret, nil, http.StatusOK, nil)
		return result.BrokerToken, result.BrokerDeviceID, nil
	}
}

// joinCall sends one join-flow request and decodes a JSON answer into out (nil: ignore it).
func joinCall(ctx context.Context, method, target string, headers map[string]string, body []byte, want int, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := joinHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

// localAddrToward is the local address this host's connections to serverURL come from: what the
// control plane will see as the peer, which a join request has to state.
func localAddrToward(ctx context.Context, serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("server URL %q: no host", serverURL)
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "", err
	}
	return host, nil
}
