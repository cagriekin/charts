package pg

import (
	"context"
	"errors"
	"fmt"
)

// RoleKind selects the privileges EnsureRole grants a chart-managed login role.
type RoleKind string

const (
	// RoleApp is the application role (postgresql.app.username): a plain login that owns
	// the chart's database, so applications stop sharing the superuser -- and with it the
	// superuser_reserved_connections pool the agent and the chart's own tooling rely on.
	// The maintenance database "postgres" (the chart default) is the exception: its owner
	// could drop it or set per-database GUCs every superuser session there inherits, so
	// there the role gets CONNECT and USAGE, CREATE on schema public instead.
	RoleApp RoleKind = "app"
	// RolePgpoolCheck is pgpool's health/streaming-check role (pgpool.checkUser.username):
	// pg_monitor for the replication-delay queries, and pg_use_reserved_connections so its
	// checks draw from reserved_connections instead of competing with application
	// sessions -- a full node must not be marked down by pgpool for being full.
	RolePgpoolCheck RoleKind = "pgpool-check"
)

// ManagedRole is one chart-managed login role.
type ManagedRole struct {
	Name     string
	Password string
	Kind     RoleKind
}

// ensureRoleSQL creates the role if absent and then asserts its whole definition on every
// run, so a role an operator pre-created or altered converges to what the chart declares:
// LOGIN, no superuser or other elevated attribute, the password from the Secret, and the
// kind's privileges. The password travels exactly as rehashSQL's does (see there): a SCRAM
// verifier computed in Go, passed through the environment, hoisted into a session GUC after
// statement logging is lowered, and quoted with format(%I, %L). VERBOSITY terse keeps the
// dynamic statement -- verifier included -- out of psql's CONTEXT lines on failure.
const ensureRoleSQL = `\set VERBOSITY terse
\getenv role_secret ENSURE_ROLE_SECRET
SET log_statement = 'none';
SET log_min_duration_statement = -1;
SET log_min_error_statement = 'panic';
SET client_min_messages = 'warning';
SET myvars.role_name = :'r';
SET myvars.role_secret = :'role_secret';
SET myvars.role_kind = :'k';
SET myvars.role_db = :'d';
DO $$
DECLARE
  v_name TEXT := current_setting('myvars.role_name');
  v_secret TEXT := current_setting('myvars.role_secret');
  v_kind TEXT := current_setting('myvars.role_kind');
  v_db TEXT := current_setting('myvars.role_db');
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_name) THEN
    EXECUTE format('CREATE ROLE %I', v_name);
  END IF;
  EXECUTE format('ALTER ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', v_name, v_secret);
  IF v_kind = 'pgpool-check' THEN
    EXECUTE format('GRANT pg_monitor, pg_use_reserved_connections TO %I', v_name);
  ELSIF v_kind = 'app' AND v_db = 'postgres' THEN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I', v_db, v_name);
    IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'public') THEN
      EXECUTE format('GRANT USAGE, CREATE ON SCHEMA public TO %I', v_name);
    END IF;
  ELSIF v_kind = 'app' THEN
    EXECUTE format('ALTER DATABASE %I OWNER TO %I', v_db, v_name);
  ELSE
    RAISE EXCEPTION 'unknown managed role kind %', v_kind;
  END IF;
END
$$;`

// EnsureRole converges one chart-managed role on the LOCAL primary, connecting as
// superUser over the local socket (pg_hba `local all all trust`) to db. Idempotent.
// ErrNeedsSASLprep is returned matchable: such a password can never be provisioned by
// this path, so the caller must not retry it every tick.
func EnsureRole(ctx context.Context, ex StdinExec, superUser, db string, r ManagedRole) error {
	if superUser == "" || db == "" || r.Name == "" || r.Password == "" {
		return fmt.Errorf("ensure role: superuser %q, database %q, role %q and its password are all required", superUser, db, r.Name)
	}
	if r.Kind != RoleApp && r.Kind != RolePgpoolCheck {
		return fmt.Errorf("ensure role %s: unknown kind %q", r.Name, r.Kind)
	}
	secret, err := ScramVerifier(r.Password)
	if err != nil {
		return fmt.Errorf("ensure role %s: %w", r.Name, err)
	}
	if err := ex.RunStdin(ctx,
		[]string{"ENSURE_ROLE_SECRET=" + secret},
		ensureRoleSQL, "psql",
		"-U", superUser, "-d", db,
		"-v", "ON_ERROR_STOP=1",
		"-v", "r="+r.Name,
		"-v", "k="+string(r.Kind),
		"-v", "d="+db,
		"--no-psqlrc"); err != nil {
		return fmt.Errorf("ensure role %s: %w", r.Name, err)
	}
	return nil
}

// IsPermanentRoleError reports whether EnsureRole failed in a way no retry can fix.
func IsPermanentRoleError(err error) bool { return errors.Is(err, ErrNeedsSASLprep) }
