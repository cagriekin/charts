package pg

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeSQLConn scripts one connection: each exec pops the next reply. Counters are guarded
// so the concurrency tests (#371) can read them under -race while another goroutine runs a
// statement; most tests touch them from one goroutine and pay nothing for it.
type fakeSQLConn struct {
	mu       sync.Mutex
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs++
	if len(f.replies) == 0 {
		return nil, errors.New("fake: no reply scripted")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r.rows, r.err
}
func (f *fakeSQLConn) closed() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.isClosed }
func (f *fakeSQLConn) close(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.isClosed = true
}
func (f *fakeSQLConn) stats() (execs, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execs, f.closes
}

// blockingConn holds its statement open until released (or the ctx ends): the shape of a
// restartpoint flushing dirty buffers, used to prove a write never holds the read session.
type blockingConn struct {
	fakeSQLConn
	started chan struct{} // closed when exec begins
	release chan struct{} // exec returns once this is closed
}

func newBlockingConn() *blockingConn {
	return &blockingConn{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingConn) exec(ctx context.Context, _ string) ([][][]byte, error) {
	b.mu.Lock()
	b.execs++
	b.mu.Unlock()
	close(b.started)
	select {
	case <-b.release:
		return [][][]byte{row("done")}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

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

// cacheWithConns is cacheWith over arbitrary sqlConn implementations (the blocking fakes).
func cacheWithConns(conns ...sqlConn) (*ConnCache, *[]string) {
	dsns := &[]string{}
	i := 0
	var mu sync.Mutex
	c := &ConnCache{
		dial: func(_ context.Context, dsn, _ string) (sqlConn, error) {
			mu.Lock()
			defer mu.Unlock()
			*dsns = append(*dsns, dsn)
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
	return c, dsns
}

// cacheWithPasswords is cacheWith that also records the password handed to each dial.
func cacheWithPasswords(conns ...*fakeSQLConn) (*ConnCache, *[]string, *[]string) {
	dsns := &[]string{}
	passwords := &[]string{}
	i := 0
	var mu sync.Mutex // the concurrency tests dial from two goroutines
	c := &ConnCache{
		dial: func(_ context.Context, dsn, password string) (sqlConn, error) {
			mu.Lock()
			defer mu.Unlock()
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
	got := connString(ConnInfo{Host: "h", Port: 5432, User: "it's me", DB: "my db", Password: `p\ss'wd`}, 7, appNameRead)
	for _, want := range []string{`host='h'`, "port=5432", `user='it\'s me'`, `dbname='my db'`, "connect_timeout=7", "application_name=pg-ha-agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %s", got, want)
		}
	}
	// The write path announces itself, so a long CHECKPOINT is attributable in pg_stat_activity
	// and the read-session count ("one per agent per node") stays exact.
	if w := connString(ci, 7, appNameWrite); !strings.Contains(w, "application_name=pg-ha-agent-write") {
		t.Errorf("write DSN lacks its application name: %s", w)
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
	// A write to the departed node (#371) runs on its own connection: it recreates no entry
	// and refreshes no lastUsed, so the sweep's view of who is idle is unchanged.
	before := c.entries[ci.Host+":5432:repmgr:repmgr"].lastUsed
	writeConn := &fakeSQLConn{replies: []fakeReply{{rows: nil}}}
	c.dial = func(_ context.Context, _, _ string) (sqlConn, error) { return writeConn, nil }
	clock = clock.Add(10 * time.Second)
	if _, err := c.QueryText(probeCtx(t), departed, QueryWrite, "CHECKPOINT;"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.entries[departed.Host+":5432:repmgr:repmgr"]; ok || len(c.entries) != 1 {
		t.Errorf("a write must not create or revive a cache entry: entries=%d", len(c.entries))
	}
	if c.entries[ci.Host+":5432:repmgr:repmgr"].lastUsed != before {
		t.Error("a write must not touch another entry's lastUsed")
	}
	if _, c2 := writeConn.stats(); c2 != 1 {
		t.Errorf("the write connection must be closed: closes=%d", c2)
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

// #371: a write never touches the cached socket. It runs on a connection dialled for it, so a
// stale cached session is irrelevant to the write, and a later read is what discovers the stale
// socket and redials, exactly as before.
func TestConnCacheWriteIgnoresTheCachedSocket(t *testing.T) {
	stale := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {err: errors.New("unexpected EOF")}}}
	writeConn := &fakeSQLConn{replies: []fakeReply{{rows: nil}}}
	fresh := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}}}
	c, dsns := cacheWith(stale, writeConn, fresh)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryWrite, "CHECKPOINT;"); err != nil {
		t.Fatalf("the write must run on its own connection regardless of the cached socket: %v", err)
	}
	if e, c2 := writeConn.stats(); e != 1 || c2 != 1 {
		t.Errorf("the write connection must run the statement once and be closed: execs=%d closes=%d", e, c2)
	}
	if _, c1 := stale.stats(); c1 != 0 || len(*dsns) != 2 {
		t.Errorf("the write must leave the cached session alone: staleCloses=%d dials=%d", c1, len(*dsns))
	}
	if got, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil || got != "t" {
		t.Fatalf("the read must find the stale socket and redial: %q %v", got, err)
	}
	if _, c1 := stale.stats(); c1 != 1 || len(*dsns) != 3 {
		t.Errorf("want the stale conn dropped by the read and one more dial: staleCloses=%d dials=%d", c1, len(*dsns))
	}
}

// #371, the point of the split: a write that holds its connection for seconds (a restartpoint
// flushing dirty buffers) never delays a read to the same node, because it is not on the read
// session. The read runs on the cached connection while the write is still in flight.
func TestConnCacheWriteDoesNotHoldTheReadSession(t *testing.T) {
	readConn := &fakeSQLConn{replies: []fakeReply{{rows: [][][]byte{row("t")}}, {rows: [][][]byte{row("f")}}}}
	writeConn := newBlockingConn()
	c, dsns := cacheWithConns(readConn, writeConn)
	if _, err := c.QueryText(probeCtx(t), ci, QueryRead, "q"); err != nil {
		t.Fatal(err)
	}
	writeErr := make(chan error, 1)
	go func() {
		_, err := c.QueryText(probeCtx(t), ci, QueryWrite, "CHECKPOINT;")
		writeErr <- err
	}()
	<-writeConn.started
	start := time.Now()
	got, err := c.QueryText(probeCtx(t), ci, QueryRead, "q")
	if err != nil || got != "f" {
		t.Fatalf("read during the write: %q %v", got, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the read waited on the write: %s", took)
	}
	close(writeConn.release)
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
	if e, _ := readConn.stats(); e != 2 || len(*dsns) != 2 {
		t.Errorf("want both reads on the cached session and exactly one extra dial for the write: readExecs=%d dials=%d", e, len(*dsns))
	}
	if _, c2 := writeConn.stats(); c2 != 1 {
		t.Errorf("the write connection must be closed after its statement: closes=%d", c2)
	}
	if e := c.entries[ci.Host+":5432:repmgr:repmgr"]; e == nil || e.conn != sqlConn(readConn) {
		t.Error("the cached entry must still hold the read connection")
	}
	if !strings.Contains((*dsns)[0], "application_name=pg-ha-agent") || strings.Contains((*dsns)[0], "-write") {
		t.Errorf("read DSN: %s", (*dsns)[0])
	}
	if !strings.Contains((*dsns)[1], "application_name=pg-ha-agent-write") {
		t.Errorf("write DSN: %s", (*dsns)[1])
	}
}

// A write that fails is not retried and its connection is closed; it leaves no cache entry
// behind and the cached read session, if any, is untouched.
func TestConnCacheWriteFailureIsNotRetriedAndClosesItsConnection(t *testing.T) {
	writeConn := &fakeSQLConn{replies: []fakeReply{{err: errors.New("unexpected EOF")}}}
	c, dsns := cacheWith(writeConn, &fakeSQLConn{})
	if _, err := c.QueryText(probeCtx(t), ci, QueryWrite, "CHECKPOINT;"); err == nil {
		t.Fatal("want the error")
	}
	if e, c2 := writeConn.stats(); e != 1 || c2 != 1 || len(*dsns) != 1 {
		t.Errorf("want one statement, one close, one dial: execs=%d closes=%d dials=%d", e, c2, len(*dsns))
	}
	if len(c.entries) != 0 {
		t.Errorf("a write must not create a cache entry: %d", len(c.entries))
	}
}

// A read that waits on a held session gives up at its deadline with an error that says so,
// still reads as a deadline to callers, and matches neither saturation nor refusal, so Probe
// reports plain "unreachable" and no saturation episode starts.
func TestConnCacheReadWaiterReportsTheHeldSession(t *testing.T) {
	holder := newBlockingConn()
	c, dsns := cacheWithConns(holder, &fakeSQLConn{})
	holderErr := make(chan error, 1)
	go func() {
		_, err := c.QueryText(probeCtx(t), ci, QueryRead, "q")
		holderErr <- err
	}()
	<-holder.started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.QueryText(ctx, ci, QueryRead, "q")
	if took := time.Since(start); took > time.Second {
		t.Errorf("the waiter outlived its deadline: %s", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error, got %v", err)
	}
	if !strings.Contains(err.Error(), "held by another query") || !strings.Contains(err.Error(), ci.Host) {
		t.Errorf("the error must name the held session: %v", err)
	}
	if IsTooManyConnections(err) || IsConnRefused(err) {
		t.Error("a held session is neither saturation nor refusal")
	}
	if len(*dsns) != 1 {
		t.Errorf("the waiter must not dial: dials=%d", len(*dsns))
	}
	close(holder.release)
	if err := <-holderErr; err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// Close waits for a write in flight up to its own deadline, reports it when it does not finish,
// and does not pull the connection from under the statement; the write path closes it.
func TestConnCacheCloseWaitsForAWriteInFlightUpToItsDeadline(t *testing.T) {
	writeConn := newBlockingConn()
	c, _ := cacheWithConns(writeConn)
	writeErr := make(chan error, 1)
	go func() {
		_, err := c.QueryText(probeCtx(t), ci, QueryWrite, "CHECKPOINT;")
		writeErr <- err
	}()
	<-writeConn.started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Close(ctx)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Close outlived its deadline: %s", took)
	}
	if err == nil || !strings.Contains(err.Error(), "writes in flight") {
		t.Errorf("Close must report the write still running: %v", err)
	}
	if _, c2 := writeConn.stats(); c2 != 0 {
		t.Error("Close must not close a connection under a running statement")
	}
	close(writeConn.release)
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, c2 := writeConn.stats(); c2 != 1 {
		t.Errorf("the write path must close its connection: closes=%d", c2)
	}
}

func TestConnCacheWriteAfterCloseIsRefused(t *testing.T) {
	c, dsns := cacheWith(&fakeSQLConn{})
	if err := c.Close(probeCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryText(probeCtx(t), ci, QueryWrite, "CHECKPOINT;"); err == nil || len(*dsns) != 0 {
		t.Errorf("a write after Close must be refused without dialling: err=%v dials=%d", err, len(*dsns))
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
