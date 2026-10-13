package pg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSQLConn scripts one connection: each exec pops the next reply.
type fakeSQLConn struct {
	replies  []fakeReply
	execs    int
	isClosed bool
	closes   int
}

type fakeReply struct {
	rows [][][]byte
	err  error
}

func (f *fakeSQLConn) exec(_ context.Context, _ string) ([][][]byte, error) {
	f.execs++
	if len(f.replies) == 0 {
		return nil, errors.New("fake: no reply scripted")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r.rows, r.err
}
func (f *fakeSQLConn) closed() bool          { return f.isClosed }
func (f *fakeSQLConn) close(context.Context) { f.closes++; f.isClosed = true }

func row(cells ...string) [][]byte {
	out := make([][]byte, len(cells))
	for i, c := range cells {
		if c == "<NULL>" {
			continue
		}
		out[i] = []byte(c)
	}
	return out
}

// cacheWith returns a cache whose dial hands out the given connections in order and
// records every DSN it was asked for.
func cacheWith(conns ...*fakeSQLConn) (*ConnCache, *[]string) {
	dsns := &[]string{}
	i := 0
	c := &ConnCache{dial: func(_ context.Context, dsn string) (sqlConn, error) {
		*dsns = append(*dsns, dsn)
		if i >= len(conns) {
			return nil, errors.New("fake: no more connections")
		}
		conn := conns[i]
		i++
		return conn, nil
	}}
	return c, dsns
}

var ci = ConnInfo{Host: "pg-1.pg-headless", Port: 5432, User: "repmgr", DB: "repmgr", Password: "s3cret"}

// #364: the point of the cache -- consecutive probes share one connection and one dial.
func TestConnCacheReusesTheConnectionAcrossProbes(t *testing.T) {
	conn := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {rows: [][][]byte{row("0/3000060")}}}}
	c, dsns := cacheWith(conn)
	for _, want := range []string{"t", "0/3000060"} {
		got, err := c.QueryText(context.Background(), ci, "SELECT 1")
		if err != nil || got != want {
			t.Fatalf("got %q err=%v, want %q", got, err, want)
		}
	}
	if len(*dsns) != 1 || conn.execs != 2 {
		t.Errorf("want one dial and two execs on it, got %d dials, %d execs", len(*dsns), conn.execs)
	}
}

// A reused socket that fails (the peer restarted since the last tick) is dropped and the
// query retried ONCE on a fresh dial, so the probe reports what a fresh psql would have.
func TestConnCacheRetriesOnceOnAStaleConnection(t *testing.T) {
	stale := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {err: errors.New("unexpected EOF")}}}
	fresh := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("f")}}}}
	c, dsns := cacheWith(stale, fresh)
	if got, _ := c.QueryText(context.Background(), ci, "q"); got != "t" {
		t.Fatalf("first probe: %q", got)
	}
	got, err := c.QueryText(context.Background(), ci, "q")
	if err != nil || got != "f" {
		t.Fatalf("after the stale failure want the fresh answer, got %q err=%v", got, err)
	}
	if stale.closes != 1 || len(*dsns) != 2 || fresh.execs != 1 {
		t.Errorf("stale must be closed once and replaced by one fresh dial: closes=%d dials=%d freshExecs=%d", stale.closes, len(*dsns), fresh.execs)
	}
}

// A failure on a connection dialled for THIS probe is the node's answer: no second dial,
// so a dead peer costs one timeout, not two.
func TestConnCacheDoesNotRetryAFreshDial(t *testing.T) {
	fresh := &fakeSQLConn{replies: []fakeReply{{err: errors.New("connection refused")}}}
	c, dsns := cacheWith(fresh, &fakeSQLConn{})
	if _, err := c.QueryText(context.Background(), ci, "q"); err == nil {
		t.Fatal("want the error")
	}
	if len(*dsns) != 1 || fresh.closes != 1 {
		t.Errorf("want exactly one dial and the failed conn closed: dials=%d closes=%d", len(*dsns), fresh.closes)
	}
}

// A connection pgconn already closed (its own deadline cancel) is not reused.
func TestConnCacheRedialsAClosedConnection(t *testing.T) {
	first := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	second := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	c, dsns := cacheWith(first, second)
	c.QueryText(context.Background(), ci, "q")
	first.isClosed = true
	if _, err := c.QueryText(context.Background(), ci, "q"); err != nil {
		t.Fatal(err)
	}
	if len(*dsns) != 2 || second.execs != 1 {
		t.Errorf("want a redial: dials=%d secondExecs=%d", len(*dsns), second.execs)
	}
}

// The caller's deadline bounds the dial too (connect_timeout mirrors the remaining
// budget), and a probe that timed out is not retried on a new socket.
func TestConnCacheDialHonoursTheCallerDeadline(t *testing.T) {
	stale := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {err: context.DeadlineExceeded}}}
	c, dsns := cacheWith(stale, &fakeSQLConn{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.QueryText(ctx, ci, "q")
	if !strings.Contains((*dsns)[0], "connect_timeout=3") && !strings.Contains((*dsns)[0], "connect_timeout=2") {
		t.Errorf("connect_timeout must follow the caller's deadline: %s", (*dsns)[0])
	}
	expired, ecancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer ecancel()
	time.Sleep(time.Millisecond)
	if _, err := c.QueryText(expired, ci, "q"); err == nil {
		t.Fatal("want the deadline error")
	}
	if len(*dsns) != 1 {
		t.Errorf("an expired probe must not redial: dials=%d", len(*dsns))
	}
}

// connString quotes every value: a name with a space or quote cannot inject keywords.
func TestConnStringQuotesValues(t *testing.T) {
	got := connString(ConnInfo{Host: "h", Port: 5432, User: "it's me", DB: "my db", Password: `p\ss'wd`}, 7)
	for _, want := range []string{`host='h'`, "port=5432", `user='it\'s me'`, `dbname='my db'`, "connect_timeout=7", "application_name=pg-ha-agent", `password='p\\ss\'wd'`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %s", got, want)
		}
	}
	if strings.Contains(connString(ConnInfo{Host: "h", User: "u", DB: "d"}, 1), "password=") {
		t.Error("an empty password must leave the keyword out so PGPASSFILE applies")
	}
}

// formatRows renders what `psql -tA` printed: '|' between columns, newline between rows,
// NULL empty, the whole thing trimmed -- the shape every parser in this package expects.
func TestFormatRowsMatchesPsql(t *testing.T) {
	cases := []struct {
		rows [][][]byte
		want string
	}{
		{nil, ""},
		{[][][]byte{row("t")}, "t"},
		{[][][]byte{row("0000000A", "16/B374D848")}, "0000000A|16/B374D848"},
		{[][][]byte{row("pg-1", "<NULL>", "streaming"), row("pg-2", "slot", "catchup")}, "pg-1||streaming\npg-2|slot|catchup"},
	}
	for _, c := range cases {
		if got := formatRows(c.rows); got != c.want {
			t.Errorf("formatRows = %q, want %q", got, c.want)
		}
	}
}
