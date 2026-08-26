package mechanism

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
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
	cmd.Env = append(os.Environ(), env...)
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
	ct := int(c.ConnectTimeout.Seconds())
	if ct <= 0 {
		ct = 10
	}
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s connect_timeout=%d", c.Host, c.port(), c.User, c.DB, ct)
}
