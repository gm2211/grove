package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

var (
	brokerListen       string
	brokerServer       string
	brokerCADir        string
	brokerSyncInterval time.Duration
)

var brokerCmd = &cobra.Command{
	Use:   "broker",
	Short: "Run this worker's credential broker: the proxy job VMs use, which swaps placeholders for real tokens.",
	Long: `Run this worker's credential broker.

Job VMs on this Mac send their traffic through the broker (HTTPS_PROXY). Requests to GitHub,
Anthropic and OpenAI carry a placeholder ("gph1_…"); the broker swaps it for the real token,
and masks that token in the response, so a job never holds a real credential. Everything else
is tunnelled, to public internet addresses only.

The broker pulls this worker's entries from the control plane with a device credential that
has only the broker scope and is named after the worker. ` + "`grove setup`" + ` gets one when the
control plane approves this Mac, keeps it in a 0600 file under ~/.config/grove/credentials, and
installs the broker as a boot-time LaunchDaemon that runs as the installing user
(com.grove.broker), so there is normally nothing to run by hand.

It listens on the VM bridge address (vmnet's gateway, 192.168.64.1 by default) on port 3128,
waits for that address to exist (vmnet creates the bridge with the first VM), and refuses any
address that isn't private.`,
	Args: cobra.NoArgs,
	RunE: runBroker,
}

func init() {
	brokerCmd.Flags().StringVar(&brokerListen, "listen", "", "IP address to listen on, always on port "+strconv.Itoa(broker.Port)+" (default: the vmnet gateway)")
	brokerCmd.Flags().StringVar(&brokerServer, "server", "", "Grove server URL (default: server.url from the grove config)")
	brokerCmd.Flags().StringVar(&brokerCADir, "ca-dir", "", "directory holding the broker CA (default: <grove config dir>/broker)")
	brokerCmd.Flags().DurationVar(&brokerSyncInterval, "sync-interval", broker.DefaultSyncInterval, "how often to pull entries from the control plane")
	Root.AddCommand(brokerCmd)
}

func runBroker(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listen, err := brokerListenAddr(ctx, brokerListen)
	if err != nil {
		return err
	}
	if err := broker.CheckListenAddr(listen.Addr()); err != nil {
		return err
	}
	caDir := brokerCADir
	if caDir == "" {
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		caDir = filepath.Join(filepath.Dir(path), "broker")
	}
	ca, err := broker.LoadOrCreateCA(caDir)
	if err != nil {
		return err
	}
	serverURL := brokerServer
	if serverURL == "" {
		if cfg, _, err := config.Load(); err == nil {
			serverURL = cfg.Server.URL
		}
	}
	if serverURL == "" {
		return errors.New("no control plane to sync with: pass --server, or set server.url in the grove config")
	}

	store := broker.NewStore(0)
	syncer := &broker.Syncer{Fetch: brokerFetch(serverURL, config.ResolveBrokerToken), Store: store, Interval: brokerSyncInterval}
	proxy := &broker.Proxy{Store: store, CA: ca, Resync: syncer.Sync, Synced: syncer.Synced}
	go syncer.Run(ctx)

	slog.Info("broker: starting", "listen", listen, "ca", filepath.Join(caDir, "broker-ca.pem"))
	for ctx.Err() == nil {
		if err := waitForLocalAddr(ctx, listen.Addr()); err != nil {
			return nil
		}
		l, err := net.Listen("tcp", listen.String())
		if err != nil {
			slog.Warn("broker: listen failed; retrying", "listen", listen, "err", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		slog.Info("broker: listening", "listen", listen)
		lctx, cancel := context.WithCancel(ctx)
		go func() {
			// vmnet removes the bridge when its last VM stops; start over and wait for it again.
			for lctx.Err() == nil {
				sleepCtx(lctx, 5*time.Second)
				if ok, _ := broker.HasLocalAddr(listen.Addr()); !ok {
					slog.Info("broker: bridge address went away; waiting for it", "addr", listen.Addr())
					cancel()
				}
			}
		}()
		err = proxy.Serve(lctx, l)
		cancel()
		l.Close()
		if err != nil && ctx.Err() == nil {
			slog.Warn("broker: proxy stopped; restarting", "err", err)
		}
	}
	return nil
}

// brokerFetch pulls this worker's entries from serverURL with the broker credential token
// returns. It reads the credential on first use and keeps it once found: until `grove setup` has
// stored one, every sync fails (and says why) but the broker keeps serving, so job VMs still get
// its CA and plain tunnels. Setup restarts the broker when it stores a new credential.
func brokerFetch(serverURL string, token func(context.Context) (string, error)) func(context.Context) ([]broker.Entry, error) {
	var mu sync.Mutex
	var client *apiclient.Client
	return func(ctx context.Context) ([]broker.Entry, error) {
		mu.Lock()
		if client == nil {
			t, err := token(ctx)
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			client = apiclient.New(serverURL, t)
		}
		c := client
		mu.Unlock()
		return c.BrokerEntries(ctx)
	}
}

// brokerListenAddr is --listen, or vmnet's gateway, on broker.Port. The port is fixed because the
// job VMs' firewalls open exactly that one.
func brokerListenAddr(ctx context.Context, flag string) (netip.AddrPort, error) {
	if flag != "" {
		a, err := netip.ParseAddr(flag)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("--listen %q: want an IP address (the port is always %d): %w", flag, broker.Port, err)
		}
		return netip.AddrPortFrom(a, broker.Port), nil
	}
	gw, err := install.VMNetGateway(ctx, install.ExecRunner{})
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(gw, broker.Port), nil
}

// waitForLocalAddr returns once an interface holds a, or ctx's error once it is done.
func waitForLocalAddr(ctx context.Context, a netip.Addr) error {
	logged := false
	for {
		if ok, err := broker.HasLocalAddr(a); err == nil && ok {
			return nil
		}
		if !logged {
			slog.Info("broker: waiting for the VM bridge address (vmnet creates it with the first VM)", "addr", a)
			logged = true
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
