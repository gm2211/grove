package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
	"github.com/gm2211/grove/internal/server"
)

// brokerIssuer mints a device credential; apiclient.Client.IssueDevice in production.
type brokerIssuer func(ctx context.Context, name string, scopes []string, expiresIn time.Duration) (string, server.Principal, error)

// controlPlaneBrokerToken gives the worker on the control-plane host its broker credential. A
// worker anywhere else gets one from join approval (`grove setup`), but the control plane's own
// worker is installed with `grove install --role worker` and never joins, so its broker ran with
// no credential and could never sync (doctor: "no broker credential file"). This host already
// holds the operator token, so it mints the credential the join would have: scope broker only,
// named after the Orchard worker, exactly what GET /broker/entries demands.
//
// It returns "" (and the install goes on without one, as before) when this isn't the control
// plane, a credential is already stored, or the control plane can't issue one; the last says so.
func controlPlaneBrokerToken(ctx context.Context, cfg *config.Config, credential, worker string, issue brokerIssuer, warn io.Writer) string {
	if cfg == nil || cfg.Server.Listen == "" || cfg.Server.URL == "" || worker == "" {
		return ""
	}
	if _, err := os.Lstat(credential); !errors.Is(err, os.ErrNotExist) {
		return "" // stored already (or unreadable: the broker-credential step and doctor report that)
	}
	token, device, err := issue(ctx, worker, []string{server.ScopeBroker}, 0)
	if err != nil || token == "" {
		fmt.Fprintf(warn, "warning: could not issue this control plane's own broker credential (%v), so jobs on its worker can't use credentials yet. Start `grove serve`, then run this install again.\n", err)
		return ""
	}
	fmt.Fprintf(warn, "Issued this control plane's worker %q its broker credential (device %s; revoke with `grove access revoke %s`).\n", worker, device.ID, device.ID)
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
	client := apiclient.New(cfg.Server.URL, operator)
	opts.BrokerToken = controlPlaneBrokerToken(ctx, cfg, filepath.Join(dir, config.BrokerCredentialFile), worker, client.IssueDevice, warn)
	return opts
}
