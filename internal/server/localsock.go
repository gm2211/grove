package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// ListenLocal listens on the Unix socket at path for clients on this machine, such as Argos on
// the Mac Studio: their requests never touch the network, so nothing on the tailnet, and no job
// VM behind this Mac's address, can reach them. Only the given users may connect. That is
// checked against the user the kernel reports for each connection, not file modes, so the socket
// is world-connectable (0666) and every other user's connection is closed unread. Requests on it
// still need a bearer token (see withAuth): the socket only narrows who can try.
//
// A stale socket from an earlier run is replaced; any other file at path is an error.
func ListenLocal(path string, uids []uint32, log *slog.Logger) (net.Listener, error) {
	if len(uids) == 0 {
		return nil, errors.New("local socket: no users allowed to connect")
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("local socket: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("local socket: remove stale %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	allow := make(map[uint32]bool, len(uids))
	for _, u := range uids {
		allow[u] = true
	}
	if log == nil {
		log = slog.Default()
	}
	return &localListener{Listener: l, allow: allow, log: log}, nil
}

// LocalUIDs resolves user names (or numeric uids) to uids for ListenLocal.
func LocalUIDs(names []string) ([]uint32, error) {
	var uids []uint32
	for _, name := range names {
		id := name
		if _, err := strconv.ParseUint(name, 10, 32); err != nil {
			u, err := user.Lookup(name)
			if err != nil {
				return nil, fmt.Errorf("local socket user %q: %w", name, err)
			}
			id = u.Uid
		}
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("local socket user %q: uid %q: %w", name, id, err)
		}
		uids = append(uids, uint32(n))
	}
	return uids, nil
}

type localListener struct {
	net.Listener
	allow map[uint32]bool
	log   *slog.Logger
}

func (l *localListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			c.Close()
			continue
		}
		uid, err := peerUID(uc)
		if err != nil {
			l.log.Warn("local socket: refusing a connection whose user can't be read", "err", err)
			c.Close()
			continue
		}
		if !l.allow[uid] {
			l.log.Warn("local socket: refusing a connection from a user not allowed to connect", "uid", uid)
			c.Close()
			continue
		}
		return c, nil
	}
}

type localSocketKey struct{}

// LocalConnContext marks requests that arrived on the local socket; set it as the http.Server's
// ConnContext for ListenLocal's listener.
func LocalConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, localSocketKey{}, true)
}

func viaLocalSocket(ctx context.Context) bool {
	v, _ := ctx.Value(localSocketKey{}).(bool)
	return v
}
