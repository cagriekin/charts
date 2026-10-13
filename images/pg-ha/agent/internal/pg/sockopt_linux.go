package pg

import (
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// setTCPUserTimeout returns a net.Dialer Control that sets TCP_USER_TIMEOUT on every
// socket it dials (see ConnCache).
func setTCPUserTimeout(d time.Duration) func(network, address string, rc syscall.RawConn) error {
	ms := int(d.Milliseconds())
	return func(network, address string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, ms)
		}); err != nil {
			return fmt.Errorf("set TCP_USER_TIMEOUT on %s: %w", address, err)
		}
		if serr != nil {
			return fmt.Errorf("set TCP_USER_TIMEOUT on %s: %w", address, serr)
		}
		return nil
	}
}
