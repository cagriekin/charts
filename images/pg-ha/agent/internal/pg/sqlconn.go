package pg

import (
	"context"
	"fmt"
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
	QueryText(ctx context.Context, ci ConnInfo, sql string) (string, error)
}

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

func (q ExecQuerier) QueryText(ctx context.Context, ci ConnInfo, sql string) (string, error) {
	to := q.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	env := []string{
		"PGPASSWORD=" + ci.Password,
		fmt.Sprintf("PGCONNECT_TIMEOUT=%d", int(to.Seconds())),
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

// ConnCache is the agent's transport: one persistent server connection per node,
// dialled on first use and kept across reconcile ticks, so a probe is one round trip on
// an open socket instead of a process start, a TCP connect, an authentication exchange
// and a backend fork (#364).
//
// Liveness is the caller's deadline, never the socket's: every query runs under the
// per-probe timeout, pgconn cancels and closes the connection when that deadline passes,
// and any error drops the cached connection. A stale socket therefore cannot hide a dead
// peer behind the timeout the old transport already had (PGCONNECT_TIMEOUT), and a peer
// that merely restarted between ticks costs nothing: a query that fails on a REUSED
// connection is retried once on a fresh dial within the same call, so one tick's view
// is what a fresh psql would have seen.
//
// Connections are dialled from the same libpq environment psql used -- PGSSLMODE,
// PGSSLROOTCERT, PGPASSFILE (the agent's own .pgpass) and friends are honoured by
// pgconn's config parser -- with host, port, user, database and connect_timeout set
// explicitly and the password from ConnInfo when it carries one. Sessions identify as
// application_name=pg-ha-agent in pg_stat_activity.
type ConnCache struct {
	dial func(ctx context.Context, dsn string) (sqlConn, error)

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

// cacheEntry serialises use of one connection: pgconn connections are not safe for
// concurrent use, and the control API can probe the same node the reconcile loop is
// probing. Holding the entry lock for the whole query keeps the two in order.
type cacheEntry struct {
	mu   sync.Mutex
	conn sqlConn
}

// NewConnCache returns a cache that dials real PostgreSQL connections.
func NewConnCache() *ConnCache {
	return &ConnCache{dial: func(ctx context.Context, dsn string) (sqlConn, error) {
		cfg, err := pgconn.ParseConfig(dsn)
		if err != nil {
			return nil, err
		}
		c, err := pgconn.ConnectConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return pgConn{c: c}, nil
	}}
}

// QueryText implements Querier. The deadline is the caller's (Prober sets it from its
// Timeout); connect_timeout mirrors it so a dial never outlives the probe either.
func (c *ConnCache) QueryText(ctx context.Context, ci ConnInfo, sql string) (string, error) {
	key := ci.Host + ":" + strconv.Itoa(ci.Port) + ":" + ci.User + ":" + ci.DB
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*cacheEntry{}
	}
	e := c.entries[key]
	if e == nil {
		e = &cacheEntry{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	reused := e.conn != nil && !e.conn.closed()
	if !reused {
		if err := c.redial(ctx, e, ci); err != nil {
			return "", err
		}
	}
	rows, err := e.conn.exec(ctx, sql)
	if err != nil {
		c.drop(ctx, e)
		if !reused || ctx.Err() != nil {
			return "", err
		}
		// A reused socket failed: the peer may simply have restarted since the last tick.
		// One fresh dial inside the same deadline, so this probe reports what a fresh
		// connection sees rather than a stale one's error.
		if rerr := c.redial(ctx, e, ci); rerr != nil {
			return "", rerr
		}
		if rows, err = e.conn.exec(ctx, sql); err != nil {
			c.drop(ctx, e)
			return "", err
		}
	}
	return formatRows(rows), nil
}

func (c *ConnCache) redial(ctx context.Context, e *cacheEntry, ci ConnInfo) error {
	to := 10 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < to {
			to = rem
		}
	}
	secs := int(to.Seconds())
	if secs < 1 {
		secs = 1
	}
	conn, err := c.dial(ctx, connString(ci, secs))
	if err != nil {
		return err
	}
	e.conn = conn
	return nil
}

func (c *ConnCache) drop(ctx context.Context, e *cacheEntry) {
	if e.conn != nil {
		e.conn.close(ctx)
		e.conn = nil
	}
}

// Close drops every cached connection (shutdown).
func (c *ConnCache) Close(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		e.mu.Lock()
		c.drop(ctx, e)
		e.mu.Unlock()
	}
}

// connString builds the libpq keyword/value string for ci. Values are quoted the way
// libpq and pgconn expect (backslash-escaped, single-quoted), so a database or user name
// with a space or quote cannot inject keywords -- the conninfo-injection class the agent
// guards the Lease holder name against (#298) applies here too.
func connString(ci ConnInfo, connectTimeoutSecs int) string {
	parts := []string{
		"host=" + quoteConnValue(ci.Host),
		"port=" + strconv.Itoa(ci.Port),
		"user=" + quoteConnValue(ci.User),
		"dbname=" + quoteConnValue(ci.DB),
		"connect_timeout=" + strconv.Itoa(connectTimeoutSecs),
		"application_name=pg-ha-agent",
	}
	if ci.Password != "" {
		parts = append(parts, "password="+quoteConnValue(ci.Password))
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
