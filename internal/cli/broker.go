package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/config"
)

var (
	brokerListen       string
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
has only the broker scope and is named after the worker:

  grove access issue --name <worker> --scope broker

It listens on the VM bridge address (vmnet's gateway, 192.168.64.1 by default) on port 3128,
waits for that address to exist (vmnet creates the bridge with the first VM), and refuses any
address that isn't private.`,
	Args: cobra.NoArgs,
	RunE: runBroker,
}

func init() {
	brokerCmd.Flags().StringVar(&brokerListen, "listen", "", "address to listen on (default: the vmnet gateway, port "+strconv.Itoa(broker.Port)+")")
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
	client, err := newAPIClient()
	if err != nil {
		return err
	}

	store := broker.NewStore(0)
	syncer := &broker.Syncer{Fetch: client.BrokerEntries, Store: store, Interval: brokerSyncInterval}
	proxy := &broker.Proxy{Store: store, CA: ca, Resync: syncer.Sync}
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

// brokerListenAddr parses --listen, or builds the default: vmnet's gateway on broker.Port.
func brokerListenAddr(ctx context.Context, flag string) (netip.AddrPort, error) {
	if flag != "" {
		ap, err := netip.ParseAddrPort(flag)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("--listen %q: want an IP literal and port: %w", flag, err)
		}
		return ap, nil
	}
	gw, err := vmnetGateway(ctx)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(gw, broker.Port), nil
}

// vmnetGateway is the host's address on vmnet's shared network: Shared_Net_Address from the Mac's
// vmnet preferences when set, broker.DefaultGateway otherwise.
func vmnetGateway(ctx context.Context) (netip.Addr, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "/Library/Preferences/SystemConfiguration/com.apple.vmnet", "Shared_Net_Address").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return broker.DefaultGateway, nil
		}
		return netip.Addr{}, fmt.Errorf("read vmnet settings: %w", err)
	}
	a, err := netip.ParseAddr(strings.TrimSpace(string(out)))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("vmnet Shared_Net_Address %q: %w", strings.TrimSpace(string(out)), err)
	}
	return a, nil
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
