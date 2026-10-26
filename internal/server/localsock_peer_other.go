//go:build !darwin && !linux

package server

import (
	"errors"
	"net"
)

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("local socket peer credentials are not supported on this OS")
}
