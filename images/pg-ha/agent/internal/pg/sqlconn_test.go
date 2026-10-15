package pg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
// records every DSN it was asked for. Idle eviction is effectively off (one hour on the
// real clock); the eviction tests set their own clock.
func cacheWith(conns ...*fakeSQLConn) (*ConnCache, *[]string) {
	c, dsns, _ := cacheWithPasswords(conns...)
	return c, dsns
}

// cacheWithPasswords is cacheWith that also records the password handed to each dial.
func cacheWithPasswords(conns ...*fakeSQLConn) (*ConnCache, *[]string, *[]string) {
	dsns := &[]string{}
	passwords := &[]string{}
	i := 0
	c := &ConnCache{
		dial: func(_ context.Context, dsn, password string) (sqlConn, error) {
			*dsns = append(*dsns, dsn)
			*passwords = append(*passwords, password)
			if i >= len(conns) {
				return nil, errors.New("fake: no more connections")
			}
			conn := conns[i]
			i++
			return conn, nil
		},
		idleTTL: time.Hour,
		now:     time.Now,
	}
	return c, dsns, passwords
}

func probeCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var ci = ConnInfo{Host: "pg-1.pg-headless", Port: 5432, User: "repmgr", DB: "repmgr", Password: "s3cret"}

// #364: the point of the cache -- consecutive probes share one connection and one dial.
func TestConnCacheReusesTheConnectionAcrossProbes(t *testing.T) {
	conn := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {rows: [][][]byte{row("0/3000060")}}}}
	c, dsns := cacheWith(conn)
	for _, want := range []string{"t", "0/3000060"} {
		got, err := c.QueryText(probeCtx(t), ci, QueryRead, "SELECT 1")
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
	if got, _ := c.QueryText(probeCtx(t), ci, QueryRead, "q"); got != "t" {
		t.Fatalf("first probe: %q", got)
	}
	got, err := c.QueryText(probeCtx(t), ci, QueryRead, "q")
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
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err == nil {
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
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	first.isClosed = true
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
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
	if _, err := c.QueryText(ctx, ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*dsns)[0], "connect_timeout=3") && !strings.Contains((*dsns)[0], "connect_timeout=2") {
		t.Errorf("connect_timeout must follow the caller's deadline: %s", (*dsns)[0])
	}
	expired, ecancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer ecancel()
	time.Sleep(time.Millisecond)
	if _, err := c.QueryText(expired, ci, QueryRead, "q"); err == nil {
		t.Fatal("want the deadline error")
	}
	if len(*dsns) != 1 {
		t.Errorf("an expired probe must not redial: dials=%d", len(*dsns))
	}
}

// connString quotes every value: a name with a space or quote cannot inject keywords.
func TestConnStringQuotesValues(t *testing.T) {
	got := connString(ConnInfo{Host: "h", Port: 5432, User: "it's me", DB: "my db", Password: `p\ss'wd`}, 7)
	for _, want := range []string{`host='h'`, "port=5432", `user='it\'s me'`, `dbname='my db'`, "connect_timeout=7", "application_name=pg-ha-agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %s", got, want)
		}
	}
}

// The password never enters the connection string, whose parse errors pgx redacts only
// partly; it reaches dial separately and is set on the parsed config.
func TestConnCacheKeepsThePasswordOutOfTheDSN(t *testing.T) {
	c, dsns, passwords := cacheWithPasswords(&fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}})
	withQuote := ci
	withQuote.Password = `a\'b c`
	if _, err := c.QueryText(probeCtx(t), withQuote, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*dsns)[0], "password") || strings.Contains((*dsns)[0], "b c") {
		t.Errorf("the DSN carries the password: %s", (*dsns)[0])
	}
	if (*passwords)[0] != withQuote.Password {
		t.Errorf("dial got password %q, want %q", (*passwords)[0], withQuote.Password)
	}
}

// pgconn.ParseConfig errors quote the connection string; with the password kept out of
// it, nothing secret can appear in one.
func TestNewConnCacheParseErrorCannotLeakThePassword(t *testing.T) {
	const secret = `a\'b c-SECRET`
	_, err := NewConnCache().dial(context.Background(), "host='h' sslmode='bogus'", secret)
	if err == nil {
		t.Fatal("want a parse error for an invalid sslmode")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("parse error leaks the password: %v", err)
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

// A server ERROR on a healthy reused connection is the node's answer: the session stays
// open and cached, and the statement is not re-run on a fresh dial.
func TestConnCacheKeepsTheConnectionOnAServerError(t *testing.T) {
	conn := &fakeSQLConn{replies: []fakeReply{
		{rows: [][][]byte{row("t")}},
		{err: &pgconn.PgError{Severity: "ERROR", Code: "42501", Message: "permission denied"}},
		{rows: [][][]byte{row("f")}},
	}}
	c, dsns := cacheWith(conn, &fakeSQLConn{})
	if got, _ := c.QueryText(probeCtx(t), ci, QueryRead, "q"); got != "t" {
		t.Fatalf("first probe: %q", got)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err == nil {
		t.Fatal("want the server error")
	}
	if got, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil || got != "f" {
		t.Fatalf("third probe on the same connection: %q err=%v", got, err)
	}
	if len(*dsns) != 1 || conn.closes != 0 || conn.execs != 3 {
		t.Errorf("want one dial, no close, three execs: dials=%d closes=%d execs=%d", len(*dsns), conn.closes, conn.execs)
	}
}

// A waiter behind a query hung on the same node gives up at its own deadline.
func TestConnCacheWaiterHonoursItsDeadline(t *testing.T) {
	c, _ := cacheWith(&fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}})
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	var e *cacheEntry
	for _, v := range c.entries {
		e = v
	}
	e.sem <- struct{}{}
	defer e.unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.QueryText(ctx, ci, QueryRead, "q"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded while the entry is held, got %v", err)
	}
}

// Shutdown terminates every idle cached connection and refuses further queries.
func TestConnCacheCloseTerminatesConnectionsAndRefusesUse(t *testing.T) {
	a := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	b := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	c, _ := cacheWith(a, b)
	other := ci
	other.Host = "pg-2.pg-headless"
	for _, n := range []ConnInfo{ci, other} {
		if _, err := c.QueryText(probeCtx(t), n, QueryRead, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a.closes != 1 || b.closes != 1 {
		t.Errorf("want each connection closed once, got %d and %d", a.closes, b.closes)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "SELECT 1"); err == nil {
		t.Error("a query after Close was accepted")
	}
}

// An entry held by a hung query must not stall shutdown past its deadline, and must not
// hold the cache lock while waiting.
func TestConnCacheCloseHonoursItsDeadline(t *testing.T) {
	conn := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	c, _ := cacheWith(conn)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	e := c.entries[ci.Host+":5432:repmgr:repmgr"]
	if err := e.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Close(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("want a deadline error naming the held entry, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited past its deadline on a held entry")
	}
	if conn.closes != 0 {
		t.Error("a connection still in use was closed underneath its query")
	}
}

// A node the agent stopped probing (a scaled-down ordinal, a departed upstream) is closed
// and forgotten once idle past idleTTL; nodes still probed are untouched.
func TestConnCacheEvictsIdleEntries(t *testing.T) {
	gone := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	live := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {rows: [][][]byte{row("t")}}, {rows: [][][]byte{row("t")}}}}
	c, _ := cacheWith(gone, live)
	clock := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return clock }
	c.idleTTL = time.Minute
	departed := ci
	departed.Host = "pg-2.pg-headless"
	if _, err := c.QueryText(probeCtx(t), departed, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(30 * time.Second)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if gone.closes != 0 || len(c.entries) != 2 {
		t.Fatalf("an entry idle less than idleTTL was evicted: closes=%d entries=%d", gone.closes, len(c.entries))
	}

	clock = clock.Add(31 * time.Second)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	c.evicting.Wait() // the close runs in the background
	if gone.closes != 1 {
		t.Errorf("the idle entry's connection was not closed: closes=%d", gone.closes)
	}
	if _, ok := c.entries[departed.Host+":5432:repmgr:repmgr"]; ok {
		t.Error("the idle entry is still cached")
	}
	if _, ok := c.entries[ci.Host+":5432:repmgr:repmgr"]; !ok {
		t.Error("the entry in use was evicted")
	}
}

// An idle entry whose lock is held (a query hung past idleTTL) is skipped, not waited on.
func TestConnCacheEvictionSkipsAnEntryInUse(t *testing.T) {
	held := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	other := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	c, _ := cacheWith(held, other)
	clock := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return clock }
	c.idleTTL = time.Minute
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	e := c.entries[ci.Host+":5432:repmgr:repmgr"]
	if err := e.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.unlock()

	clock = clock.Add(2 * time.Minute)
	elsewhere := ci
	elsewhere.Host = "pg-2.pg-headless"
	done := make(chan error, 1)
	go func() {
		_, err := c.QueryText(probeCtx(t), elsewhere, QueryRead, "q")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep waited on an entry in use")
	}
	if held.closes != 0 {
		t.Error("a connection in use was closed by the sweep")
	}
	if _, ok := c.entries[ci.Host+":5432:repmgr:repmgr"]; !ok {
		t.Error("an entry in use was removed")
	}
}

// A ConnCache not built by NewConnCache fails instead of running without a dialer,
// clock or idle bound.
func TestConnCacheRequiresInitialisation(t *testing.T) {
	if _, err := (&ConnCache{}).QueryText(context.Background(), ci, QueryRead, "q"); err == nil {
		t.Error("a zero ConnCache ran a query")
	}
}

// A server ERROR on the retry's fresh dial is the node's answer and leaves that session
// usable: it stays cached instead of costing another dial on the next probe.
func TestConnCacheKeepsTheFreshConnectionOnAServerErrorAfterRetry(t *testing.T) {
	stale := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {err: errors.New("unexpected EOF")}}}
	fresh := &fakeSQLConn{replies: []fakeReply{
		{err: &pgconn.PgError{Severity: "ERROR", Code: "42710", Message: "duplicate"}},
		{rows: [][][]byte{row("f")}},
	}}
	c, dsns := cacheWith(stale, fresh)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); !IsDuplicateSlot(err) {
		t.Fatalf("want the server error from the retry, got %v", err)
	}
	if got, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil || got != "f" {
		t.Fatalf("next probe on the fresh connection: %q err=%v", got, err)
	}
	if fresh.closes != 0 || len(*dsns) != 2 {
		t.Errorf("want the fresh connection kept: closes=%d dials=%d", fresh.closes, len(*dsns))
	}
}

// A query that was waiting on its entry while Close swept it must not dial a session
// Close will never terminate.
func TestConnCacheQueryWaitingThroughCloseDoesNotDial(t *testing.T) {
	conn := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	c, dsns := cacheWith(conn)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	e := c.entries[ci.Host+":5432:repmgr:repmgr"]
	if err := e.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.QueryText(probeCtx(t), ci, QueryRead, "SELECT 1")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close(probeCtx(t)) }()
	for {
		c.mu.Lock()
		closing := c.isClosed
		c.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	e.unlock()
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("a query that waited through Close was accepted")
	}
	if len(*dsns) != 1 {
		t.Errorf("want no dial after Close, got %d dials", len(*dsns))
	}
}

func TestConnCacheRequiresADeadline(t *testing.T) {
	c, dsns := cacheWith(&fakeSQLConn{})
	if _, err := c.QueryText(context.Background(), ci, QueryRead, "q"); err == nil {
		t.Fatal("a query without a deadline was accepted")
	}
	if len(*dsns) != 0 {
		t.Errorf("want no dial, got %d", len(*dsns))
	}
}

// The close of an evicted connection runs off the probe path: a probe that triggers the
// sweep is not held by a Terminate to an unrelated node, even when that close stalls.
func TestConnCacheEvictionCloseDoesNotBlockTheProbe(t *testing.T) {
	stalled := &stallingCloseConn{fakeSQLConn: fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}, release: make(chan struct{})}
	live := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {rows: [][][]byte{row("t")}}}}
	conns := []sqlConn{stalled, live}
	i := 0
	clock := time.Unix(1_700_000_000, 0)
	c := &ConnCache{
		dial: func(context.Context, string, string) (sqlConn, error) {
			conn := conns[i]
			i++
			return conn, nil
		},
		idleTTL: time.Minute,
		now:     func() time.Time { return clock },
	}
	departed := ci
	departed.Host = "pg-2.pg-headless"
	if _, err := c.QueryText(probeCtx(t), departed, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	done := make(chan error, 1)
	go func() {
		_, err := c.QueryText(probeCtx(t), ci, QueryRead, "q")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the probe waited on the close of an evicted connection")
	}
	close(stalled.release)
	c.evicting.Wait()
	if !stalled.isClosed {
		t.Error("the evicted connection was never closed")
	}
}

// stallingCloseConn blocks in close until released, standing in for a Terminate to a
// node that has stopped answering.
type stallingCloseConn struct {
	fakeSQLConn
	release chan struct{}
}

func (s *stallingCloseConn) close(ctx context.Context) {
	<-s.release
	s.fakeSQLConn.close(ctx)
}

// A write whose reused socket fails is NOT re-sent: the server may already have run it with
// the reply lost, and a second run would change the answer (a dropped slot reads as nothing
// to drop) or repeat work. The stale connection is still dropped, so the next call dials.
func TestConnCacheDoesNotRetryAWriteOnAStaleConnection(t *testing.T) {
	stale := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {err: errors.New("unexpected EOF")}}}
	fresh := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	c, dsns := cacheWith(stale, fresh)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryWrite, "CHECKPOINT;"); err == nil {
		t.Fatal("a write on a failed reused socket must return the error, not retry")
	}
	if stale.closes != 1 || len(*dsns) != 1 || fresh.execs != 0 {
		t.Errorf("want the stale conn dropped and no second dial: closes=%d dials=%d freshExecs=%d", stale.closes, len(*dsns), fresh.execs)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryWrite, "CHECKPOINT;"); err != nil {
		t.Fatalf("the next call must dial fresh and succeed: %v", err)
	}
	if len(*dsns) != 2 {
		t.Errorf("want the next call to dial: dials=%d", len(*dsns))
	}
}

// The password is required: an empty one is refused before any dial, instead of falling back
// to .pgpass or the libpq environment and surfacing as an auth failure that reads like an
// unreachable node.
func TestConnCacheRequiresAPassword(t *testing.T) {
	c, dsns := cacheWith(&fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}})
	noPassword := ci
	noPassword.Password = ""
	if _, err := c.QueryText(probeCtx(t), noPassword, QueryRead, "q"); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Fatalf("want a refusal for an empty password, got %v", err)
	}
	if len(*dsns) != 0 {
		t.Errorf("an empty password must be refused before dialling, got %d dials", len(*dsns))
	}
}
