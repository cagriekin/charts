package pg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Querier runs one SQL text against a node and returns the result the way `psql -tA`
// prints it: rows separated by newlines, columns by '|', NULL as the empty string, the
// whole thing trimmed, and only the LAST statement's result when the text holds several.
// Every parser in this package was written against that shape, so the transport
// underneath can change without touching a single parser (#364).
type Querier interface {
	QueryText(ctx context.Context, ci ConnInfo, kind QueryKind, sql string) (string, error)
}

// QueryKind selects the transport path (#371). A READ runs on the node's cached session and,
// when its reused socket fails, is retried once on a fresh dial: re-running a read is always
// safe. A WRITE (CHECKPOINT, ALTER SYSTEM, a slot create or drop) runs on a connection dialled
// for that one statement and closed after it, and is never re-sent: it may already have run
// on the server when the reply was lost, and running it again changes what the caller sees (a
// slot already dropped reads as "nothing to drop") or repeats work. Keeping writes off the
// cached session is also what keeps a probe from ever queueing behind a long statement: a
// restartpoint can hold a connection for seconds, and before the split a deadline-cut
// CHECKPOINT took the cached read session down with it (pgconn closes on deadline), so the
// next probe paid a redial. Writes are O(topology events), not O(ticks), so the dial per
// write costs nothing the #364 cost argument cared about. Classifying matters both ways: a
// read misfiled as a write pays a dial per tick; a write misfiled as a read may be re-sent.
type QueryKind int

const (
	QueryRead QueryKind = iota
	QueryWrite
)

// ExecQuerier is the original transport: a fresh `psql` process per query. It stays for
// the unit tests, whose fakes answer by inspecting the psql argument list, and as the
// documented shape of what ConnCache replaced. Not used by the agent binary: every probe
// through it paid a process start -- and in the pg-ha image `psql` on PATH is Debian's
// Perl wrapper, ~18ms of CPU before libpq even dialled -- at six to nine probes per tick
// per pod, which came to over half a core per idle pod on slow vCPUs (#364).
type ExecQuerier struct {
	Exec    Exec
	Timeout time.Duration
}

func (q ExecQuerier) QueryText(ctx context.Context, ci ConnInfo, _ QueryKind, sql string) (string, error) {
	if q.Timeout <= 0 {
		return "", fmt.Errorf("ExecQuerier: Timeout must be positive, got %s", q.Timeout)
	}
	env := []string{
		"PGPASSWORD=" + ci.Password,
		fmt.Sprintf("PGCONNECT_TIMEOUT=%d", int(q.Timeout.Seconds())),
	}
	// --no-psqlrc: every caller parses psql's OUTPUT SHAPE, and a startup file can change
	// all of it (`\pset fieldsep`, `\pset null`, a banner) while psql still exits 0 (#298).
	args := []string{"--no-psqlrc", "-h", ci.Host, "-p", strconv.Itoa(ci.Port), "-U", ci.User, "-d", ci.DB, "-tAc", sql}
	return q.Exec.Run(ctx, env, "psql", args...)
}

// sqlConn is the slice of a server connection the cache needs; pgconn implements it
// through pgConn, tests through fakes.
type sqlConn interface {
	// exec runs sql with the simple protocol and returns the LAST statement's rows in
	// text format (nil cell = NULL). Multi-statement texts are allowed, as in `psql -c`.
	exec(ctx context.Context, sql string) ([][][]byte, error)
	closed() bool
	close(ctx context.Context)
}

type pgConn struct{ c *pgconn.PgConn }

func (p pgConn) exec(ctx context.Context, sql string) ([][][]byte, error) {
	results, err := p.c.Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[len(results)-1].Rows, nil
}
func (p pgConn) closed() bool              { return p.c.IsClosed() }
func (p pgConn) close(ctx context.Context) { _ = p.c.Close(ctx) }

// ConnCache is the agent's transport: one persistent server connection per node FOR
// READS, dialled on first use and kept across reconcile ticks, so a probe is one round trip
// on an open socket instead of a process start, a TCP connect, an authentication exchange
// and a backend fork (#364). A WRITE never touches that session: it gets a connection of its
// own for the one statement (see QueryKind, #371), so the cached session is only ever shared
// between reads, which take milliseconds on a healthy node. Steady state is therefore still
// one session per agent per node -- the count the chart's superuser_reserved_connections is
// sized from -- plus, while a write runs, one short-lived extra that draws from the same
// reserve's headroom.
//
// Liveness is bounded twice. The caller's deadline bounds every query: pgconn cancels
// and closes the connection when it passes, so a stale socket cannot hide a dead peer
// behind more than the timeout the old transport already had (PGCONNECT_TIMEOUT). And
// TCP_USER_TIMEOUT (tcpUserTimeout) bounds a socket whose peer has VANISHED -- a pod
// recreated on a new IP while the old IP silently drops packets: the kernel fails the
// socket once sent data has gone unacknowledged that long, well inside the probe
// deadline, so a READ that fails on the REUSED connection is retried once on a fresh dial
// within the same call and the tick sees what a fresh psql would have seen (a write is
// not re-sent; see QueryKind). A slow query is not affected: the server still
// acknowledges what was sent.
//
// A server-side ERROR keeps the connection (the session is still usable); a FATAL or a
// transport error drops it. Entries idle longer than idleTTL -- a scaled-down ordinal, an
// upstream the agent no longer probes -- are closed and forgotten on the next query.
//
// Connections are dialled from the same libpq environment psql used -- PGSSLMODE,
// PGSSLROOTCERT and friends are honoured by pgconn's config parser -- with host, port,
// user, database and connect_timeout set explicitly and the password from ConnInfo,
// which is required (no .pgpass fallback: every caller passes the replication password
// config.Load requires, so an empty one is a bug, refused up front). The password is set on
// the parsed config, never written into the connection string, so a parse error (which
// pgx quotes with only partial password redaction) cannot leak it. Sessions identify in
// pg_stat_activity as application_name=pg-ha-agent (the cached read session) and
// pg-ha-agent-write (a write in flight), so a long CHECKPOINT is attributable at a glance.
type ConnCache struct {
	// dial opens a connection for dsn, authenticating with password (required; QueryText
	// refuses an empty one).
	dial    func(ctx context.Context, dsn, password string) (sqlConn, error)
	idleTTL time.Duration
	now     func() time.Time

	mu       sync.Mutex
	entries  map[string]*cacheEntry
	isClosed bool
	// evicting tracks background closes of evicted connections, so Close can wait for them.
	evicting sync.WaitGroup
	// writes tracks per-call write connections in flight (writeOnce), so Close can wait for
	// them -- bounded by its ctx -- instead of leaving a statement to be cut by process exit.
	writes sync.WaitGroup
}

// Application names the two transport paths present to the server (pg_stat_activity).
const (
	appNameRead  = "pg-ha-agent"
	appNameWrite = "pg-ha-agent-write"
)

// evictCloseTimeout bounds the Terminate exchange of one evicted connection. Evicted
// connections belong to nodes the agent stopped probing, so the close runs in the
// background on its own budget rather than on the deadline of whichever probe happened
// to trigger the sweep.
const evictCloseTimeout = time.Second

// connIdleTTL is how long an unused entry is kept. Every reconcile tick probes every
// current member, so only nodes the agent stopped probing reach it; a reconcile interval
// longer than this just costs one dial per tick.
const connIdleTTL = 5 * time.Minute

// tcpUserTimeout is how long sent data may go unacknowledged before the kernel fails the
// socket. On Linux it also caps the SYN retries of a connect. Short enough that a vanished
// peer fails with half the 10s probe deadline left for the redial; long enough that a
// healthy peer behind a few seconds of packet loss is not reported unreachable -- a
// result that feeds survivor ranking and failover.
const tcpUserTimeout = 5 * time.Second

// cacheEntry serialises use of one cached connection: pgconn connections are not safe for
// concurrent use. Only READS use the entry (writes dial their own connection, #371), and a
// read on a healthy node takes milliseconds, so a waiter that outlives its own deadline is
// looking at a node that is not answering reads -- the same verdict the holder is about to
// reach -- not at a busy one. Today the agent gives the lock no waiters at all: every Prober
// call runs on the reconcile goroutine, and the one parallel path, the peer-probe fan-out in
// observe(), touches a distinct key per goroutine; only Close and the idle sweep (a try-lock)
// ever contend with a query in flight. A new caller on another goroutine changes none of
// that for reads. The lock is a one-slot channel rather than a sync.Mutex so a waiter gives
// up at its own deadline instead of queueing past it behind a query hung on a dead peer.
type cacheEntry struct {
	sem  chan struct{}
	conn sqlConn
	// lastUsed is guarded by ConnCache.mu, not by sem, so the idle sweep can read it
	// without waiting on a query.
	lastUsed time.Time
}

func (e *cacheEntry) lock(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *cacheEntry) unlock() { <-e.sem }

// NewConnCache returns a cache that dials real PostgreSQL connections.
func NewConnCache() *ConnCache {
	dialer := &net.Dialer{
		// Keepalives catch a dead socket that sits idle between ticks; TCP_USER_TIMEOUT
		// also caps how long the kernel keeps probing it.
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3},
		Control:         setTCPUserTimeout(tcpUserTimeout),
	}
	return &ConnCache{
		dial: func(ctx context.Context, dsn, password string) (sqlConn, error) {
			cfg, err := pgconn.ParseConfig(dsn)
			if err != nil {
				return nil, err
			}
			// A custom DialFunc, not a tcp_user_timeout keyword: pgconn does not know that
			// keyword and would send it to the server as a run-time parameter.
			cfg.DialFunc = dialer.DialContext
			cfg.Password = password
			c, err := pgconn.ConnectConfig(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return pgConn{c: c}, nil
		},
		idleTTL: connIdleTTL,
		now:     time.Now,
	}
}

// QueryText implements Querier. The deadline is the caller's (Prober sets it from its
// Timeout); connect_timeout mirrors it so a dial never outlives the probe either.
func (c *ConnCache) QueryText(ctx context.Context, ci ConnInfo, kind QueryKind, sql string) (string, error) {
	if c.dial == nil || c.now == nil || c.idleTTL <= 0 {
		return "", errors.New("connection cache is not initialised (use NewConnCache)")
	}
	// Every caller passes the replication password, which config.Load requires. An empty
	// one must fail here rather than fall back to .pgpass or the libpq environment, where it
	// would surface as an authentication failure that reads like an unreachable node.
	if ci.Password == "" {
		return "", fmt.Errorf("connection cache: no password for %s@%s", ci.User, ci.Host)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return "", errors.New("connection cache: query context has no deadline")
	}
	if kind == QueryWrite {
		return c.writeOnce(ctx, deadline, ci, sql)
	}
	key := ci.Host + ":" + strconv.Itoa(ci.Port) + ":" + ci.User + ":" + ci.DB
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return "", errors.New("connection cache is closed")
	}
	if c.entries == nil {
		c.entries = map[string]*cacheEntry{}
	}
	now := c.now()
	evicted := c.evictIdleLocked(now, key)
	e := c.entries[key]
	if e == nil {
		e = &cacheEntry{sem: make(chan struct{}, 1)}
		c.entries[key] = e
	}
	e.lastUsed = now
	// Added under c.mu, which Close takes to set isClosed before it waits: an Add after
	// the unlock could land after Close's Wait returned and leak a close past shutdown.
	c.evicting.Add(len(evicted))
	c.mu.Unlock()
	for _, old := range evicted {
		go func(old *cacheEntry) {
			defer c.evicting.Done()
			cctx, cancel := context.WithTimeout(context.Background(), evictCloseTimeout)
			defer cancel()
			c.drop(cctx, old)
			old.unlock()
		}(old)
	}

	waitStart := c.now()
	if err := e.lock(ctx); err != nil {
		// Self-explaining in a log line: the deadline passed while another query held the
		// session, which (reads only, see cacheEntry) means that query is itself stuck on this
		// node. Wrapped, so errors.Is(err, context.DeadlineExceeded) still holds for callers
		// and neither IsTooManyConnections nor IsConnRefused matches it.
		return "", fmt.Errorf("connection cache: session to %s held by another query for %s: %w", key, c.now().Sub(waitStart).Round(time.Millisecond), err)
	}
	defer e.unlock()
	// Close may have swept this entry while we waited for it; dialling now would open a
	// session Close never sees.
	if c.closing() {
		return "", errors.New("connection cache is closed")
	}
	// A reused socket that fails may only mean the peer restarted since the last tick, so the
	// read gets one fresh dial inside the same deadline: the probe then reports what a fresh
	// connection sees rather than a stale one's error. A fresh dial is never retried. (Only
	// reads reach this loop; a write has its own path above.)
	for {
		reused := e.conn != nil && !e.conn.closed()
		if !reused {
			if err := c.redial(ctx, deadline, e, ci); err != nil {
				return "", err
			}
		}
		rows, err := e.conn.exec(ctx, sql)
		if err == nil {
			return formatRows(rows), nil
		}
		if keepOnServerError(err, e) {
			return "", err
		}
		c.drop(ctx, e)
		if !reused || ctx.Err() != nil {
			return "", err
		}
		// Close may have started while the query ran; a redial now would open a session it
		// never terminates.
		if c.closing() {
			return "", err
		}
	}
}

func (c *ConnCache) closing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isClosed
}

// writeOnce runs one write statement on a connection dialled for it and closed after it
// (#371). No cache entry, no lock, no retry: the statement cannot delay a read, cannot be
// cut short by a read's deadline, and cannot be re-sent after a lost reply. The idle sweep
// is not run and lastUsed is not touched, so a write to a departed node leaves nothing
// behind. The close runs under its own short deadline so a statement that overran the
// caller's ctx still sends Terminate instead of leaking a backend until the server notices.
func (c *ConnCache) writeOnce(ctx context.Context, deadline time.Time, ci ConnInfo, sql string) (string, error) {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return "", errors.New("connection cache is closed")
	}
	// Added under c.mu for the same reason as evicting: Close sets isClosed under this lock
	// before it waits, so an Add after the unlock could slip past its Wait.
	c.writes.Add(1)
	c.mu.Unlock()
	defer c.writes.Done()
	conn, err := c.dial(ctx, connString(ci, connectTimeoutSecs(deadline), appNameWrite), ci.Password)
	if err != nil {
		return "", err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), evictCloseTimeout)
		defer cancel()
		conn.close(cctx)
	}()
	rows, err := conn.exec(ctx, sql)
	if err != nil {
		return "", err
	}
	return formatRows(rows), nil
}

// keepOnServerError reports whether err is a server-side ERROR (permission denied, a
// RAISE, a duplicate slot) that left the session usable: such an error would recur on a
// fresh dial, so the connection is kept and the statement not retried. A FATAL (e.g.
// admin shutdown on a restarted peer) closes the session, so it is not kept.
func keepOnServerError(err error, e *cacheEntry) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && !e.conn.closed()
}

// connectTimeoutSecs mirrors the caller's deadline as libpq's connect_timeout, so a dial
// never outlives the query it serves. connect_timeout=0 means "wait forever" to libpq and
// pgconn, so an already-passed deadline floors at 1; ctx still bounds the dial.
func connectTimeoutSecs(deadline time.Time) int {
	secs := int(time.Until(deadline).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

func (c *ConnCache) redial(ctx context.Context, deadline time.Time, e *cacheEntry, ci ConnInfo) error {
	conn, err := c.dial(ctx, connString(ci, connectTimeoutSecs(deadline), appNameRead), ci.Password)
	if err != nil {
		return err
	}
	e.conn = conn
	return nil
}

// evictIdleLocked removes every entry other than keep that has been unused for idleTTL
// and is not in use, and returns them still locked so the caller can close their
// connections after releasing c.mu. An entry whose lock is held is skipped, never
// waited on: it is in use, so it will be swept on a later call once it goes idle.
// Called with c.mu held.
func (c *ConnCache) evictIdleLocked(now time.Time, keep string) []*cacheEntry {
	var out []*cacheEntry
	for k, e := range c.entries {
		if k == keep || now.Sub(e.lastUsed) < c.idleTTL {
			continue
		}
		select {
		case e.sem <- struct{}{}:
			delete(c.entries, k)
			out = append(out, e)
		default:
		}
	}
	return out
}

func (c *ConnCache) drop(ctx context.Context, e *cacheEntry) {
	if e.conn != nil {
		e.conn.close(ctx)
		e.conn = nil
	}
}

// Close terminates every cached connection cleanly (shutdown), so the nodes see a
// Terminate message instead of logging an unexpected EOF for each agent session. The
// cache refuses further queries once Close starts. The cache lock is released before
// waiting on any entry, and each wait is bounded by ctx: an entry still held by a query
// hung on a dead peer is reported, not waited out, and its socket goes with the process.
func (c *ConnCache) Close(ctx context.Context) error {
	c.mu.Lock()
	c.isClosed = true
	entries := make(map[string]*cacheEntry, len(c.entries))
	for k, e := range c.entries {
		entries[k] = e
	}
	c.mu.Unlock()

	var errs []error
	for key, e := range entries {
		if err := e.lock(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", key, err))
			continue
		}
		c.drop(ctx, e)
		e.unlock()
	}
	// A write in flight closes its own connection when its statement returns; wait for that
	// up to ctx, so a Terminate is sent rather than the socket going with the process.
	writesDone := make(chan struct{})
	go func() { c.writes.Wait(); close(writesDone) }()
	select {
	case <-writesDone:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("writes in flight: %w", ctx.Err()))
	}
	// Background closes of evicted entries are already bounded by evictCloseTimeout.
	c.evicting.Wait()
	return errors.Join(errs...)
}

// connString builds the libpq keyword/value string for ci, without the password (see
// ConnCache: it is set on the parsed config instead). Values are quoted the way
// libpq and pgconn expect (backslash-escaped, single-quoted), so a database or user name
// with a space or quote cannot inject keywords -- the conninfo-injection class the agent
// guards the Lease holder name against (#298) applies here too.
func connString(ci ConnInfo, connectTimeoutSecs int, appName string) string {
	parts := []string{
		"host=" + quoteConnValue(ci.Host),
		"port=" + strconv.Itoa(ci.Port),
		"user=" + quoteConnValue(ci.User),
		"dbname=" + quoteConnValue(ci.DB),
		"connect_timeout=" + strconv.Itoa(connectTimeoutSecs),
		"application_name=" + appName,
	}
	return strings.Join(parts, " ")
}

func quoteConnValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// formatRows renders text-format rows as `psql -tA` would print them.
func formatRows(rows [][][]byte) string {
	var b strings.Builder
	for i, row := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		for j, cell := range row {
			if j > 0 {
				b.WriteByte('|')
			}
			b.Write(cell) // nil (NULL) writes nothing, which is psql's default null display
		}
	}
	return strings.TrimSpace(b.String())
}
