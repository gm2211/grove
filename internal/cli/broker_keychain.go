package cli

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

// keychainHandoffTTL is how long the broker keeps trying to hand a collected password to the
// worker's wrapper before dropping it (the operator then types it again).
const keychainHandoffTTL = 10 * time.Minute

// keychainHandoff passes a login password the control plane handed this broker (an operator typed
// it into the panel) to the worker wrapper's FIFO (install.DeliverKeychainUnlock). It holds at most
// one, in memory only, and forgets it once delivered or after keychainHandoffTTL.
type keychainHandoff struct {
	mu       sync.Mutex
	password string
	expires  time.Time
	wake     chan struct{}
	path     func() (string, error)
	deliver  func(path, password string) error
	now      func() time.Time
	interval time.Duration
}

func newKeychainHandoff() *keychainHandoff {
	return &keychainHandoff{
		wake: make(chan struct{}, 1),
		path: func() (string, error) {
			dir, err := config.CredentialsDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(dir, install.KeychainUnlockFIFOName), nil
		},
		deliver:  install.DeliverKeychainUnlock,
		now:      time.Now,
		interval: 2 * time.Second,
	}
}

func (k *keychainHandoff) offer(password string) {
	k.mu.Lock()
	k.password, k.expires = password, k.now().Add(keychainHandoffTTL)
	k.mu.Unlock()
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// try delivers the pending password, if any, and says whether one is still pending.
func (k *keychainHandoff) try() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.password == "" {
		return false
	}
	if k.now().After(k.expires) {
		k.password = ""
		slog.Warn("broker: no worker wrapper took the keychain password in time; dropped it, so unlock again from the panel")
		return false
	}
	path, err := k.path()
	if err == nil {
		err = k.deliver(path, k.password)
	}
	switch {
	case err == nil:
		k.password = ""
		slog.Info("broker: handed the login keychain password to the worker")
		return false
	case errors.Is(err, install.ErrNoUnlockReader):
		return true
	default:
		k.password = ""
		slog.Error("broker: could not hand the keychain password to the worker; dropped it", "err", err)
		return false
	}
}

func (k *keychainHandoff) run(ctx context.Context) {
	t := time.NewTicker(k.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.wake:
		case <-t.C:
		}
		k.try()
	}
}
