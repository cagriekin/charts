package pg

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The dialer Control really sets TCP_USER_TIMEOUT on the socket, read back from the kernel.
func TestSetTCPUserTimeoutAppliesToTheSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := net.Dialer{Control: setTCPUserTimeout(3 * time.Second)}
	conn, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rc, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got != 3000 {
		t.Errorf("TCP_USER_TIMEOUT = %dms, want 3000", got)
	}
}
