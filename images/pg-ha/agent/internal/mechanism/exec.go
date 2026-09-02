package mechanism

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/cagriekin/pg-ha-agent/internal/childenv"
)

// Command execution and connection-string plumbing shared by every Mechanism
// implementation. These lived in repmgr.go until #294 deleted it; they were never
// repmgr-specific -- Native uses all of them -- so they moved here rather than being
// duplicated into native.go.

// Runner executes a command (with extra env appended) and returns its combined
// output. Injectable so a mechanism's external command calls are unit-testable.
type Runner interface {
	Run(ctx context.Context, env []string, name string, args ...string) (string, error)
}

// OSRunner is the production Runner backed by os/exec (combined stdout+stderr, so a
// failing command's own diagnostics surface in the returned error).
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Strip the agent's own credential env from what pg_basebackup/pg_rewind inherit
	// (#298 security review): they authenticate via the PGPASSWORD in env, never the
	// raw agent secrets, so those should not sit in a child's /proc/<pid>/environ.
	cmd.Env = childenv.Filtered(os.Environ(), env)
	// WaitDelay bounds the gap between killing the process and Wait returning, exactly as
	// pg.OSExec does and for the same reason (#288 review, mirrored here by #298's): a
	// cancelled context kills only the direct child, while Wait blocks on the output pipe
	// reaching EOF -- which any GRANDCHILD still holding it prevents. pg_basebackup -X stream
	// forks a WAL receiver that inherits this pipe, so a cancelled clone could hang Wait
	// forever with opMu held, starving dcs.OnLost's demote until the kubelet's SIGKILL. That
	// is the #288 fencing hazard, on the one exec path that had not been given the fix.
	//
	// Not shared with pg.OSExec despite the near-duplication: that one returns stdout ONLY
	// (its callers parse query output, so diagnostics must not be mixed in), while a
	// mechanism's callers want the failing CLI's own message, hence CombinedOutput. Folding
	// them together would force one caller class to take the wrong output shape.
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Clock returns the current time; injectable so the .diverged.<ts> suffix is
// deterministic in tests.
type Clock func() time.Time

func (c Conn) port() int {
	if c.Port == 0 {
		return 5432
	}
	return c.Port
}

// database is the dbname to connect to, defaulting to "postgres" (#289). Only the
// native slot helpers need a default: conninfo() below is built from a Conn the caller
// always populates, but a slot create must connect SOMEWHERE even when the caller had no
// replication DB to name, and "postgres" always exists.
func (c Conn) database() string {
	if c.DB == "" {
		return "postgres"
	}
	return c.DB
}

// conninfo builds a libpq conninfo string WITHOUT the password (that goes via
// PGPASSWORD), so it never lands in argv or logs.
func (c Conn) conninfo() string {
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s connect_timeout=%d", c.Host, c.port(), c.User, c.DB, c.connectTimeoutSecs())
}

// connectTimeoutSecs is the libpq connect_timeout to use for this peer, in whole seconds,
// defaulting to 10 when the caller left it unset.
//
// Used both in conninfo() (for the tools that take a conninfo string) and as PGCONNECT_TIMEOUT
// (for the ones addressed with -h/-p/-U, which would otherwise inherit libpq's default of NO
// timeout at all -- see runConn, #298 review).
func (c Conn) connectTimeoutSecs() int {
	if ct := int(c.ConnectTimeout.Seconds()); ct > 0 {
		return ct
	}
	return 10
}
