package pg

import (
	"context"
	"net"
	"testing"
	"time"
)

// A refused TCP connection (nothing listening) is reported as ConnRefused, not as
// saturation, so a saturation episode can end on it.
func TestProbeReportsARefusedConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := NewProber(NewConnCache())
	if err != nil {
		t.Fatal(err)
	}
	p.Timeout = 5 * time.Second
	ns := p.Probe(context.Background(), ConnInfo{Host: "127.0.0.1", Port: port, User: "u", DB: "d", Password: "pw"})
	if ns.Reachable || ns.Saturated || !ns.ConnRefused {
		t.Errorf("closed port: got %+v, want ConnRefused only", ns)
	}
}
