//go:build !linux

package pg

import (
	"errors"
	"syscall"
	"time"
)

// setTCPUserTimeout fails every dial off Linux: the agent only runs on Linux, and a
// connection without TCP_USER_TIMEOUT would silently lose the vanished-peer bound
// ConnCache depends on.
func setTCPUserTimeout(time.Duration) func(network, address string, rc syscall.RawConn) error {
	return func(string, string, syscall.RawConn) error {
		return errors.New("TCP_USER_TIMEOUT is only supported on linux")
	}
}
