package pg

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureRoleInvocation(t *testing.T) {
	for _, kind := range []RoleKind{RoleApp, RolePgpoolCheck} {
		f := &fakeStdinExec{}
		if err := EnsureRole(context.Background(), f, "postgres", "appdb", ManagedRole{Name: "app", Password: "s3cr3t'pw", Kind: kind}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		argline := strings.Join(f.args, " ")
		for _, want := range []string{"-U postgres", "-d appdb", "ON_ERROR_STOP=1", "r=app", "k=" + string(kind), "d=appdb", "--no-psqlrc"} {
			if !strings.Contains(argline, want) {
				t.Errorf("%s: args missing %q: %v", kind, want, f.args)
			}
		}
		// Only a SCRAM verifier travels, and only through the environment.
		if strings.Contains(argline, "s3cr3t") || strings.Contains(f.stdin, "s3cr3t") {
			t.Errorf("%s: plaintext password on argv or in the SQL", kind)
		}
		secret := ""
		for _, e := range f.env {
			if v, ok := strings.CutPrefix(e, "ENSURE_ROLE_SECRET="); ok {
				secret = v
			}
			if strings.Contains(e, "s3cr3t") {
				t.Errorf("%s: plaintext password in the environment", kind)
			}
		}
		if !strings.HasPrefix(secret, "SCRAM-SHA-256$4096:") {
			t.Errorf("%s: want a SCRAM verifier in ENSURE_ROLE_SECRET, got %q", kind, secret)
		}
	}
}

// The role definition is asserted on every run, so a pre-existing role that was a
// superuser (or otherwise elevated) is brought down to a plain login; the grants match the
// kind; and statement logging is lowered before the verifier is sent.
func TestEnsureRoleSQL(t *testing.T) {
	for _, want := range []string{
		`\set VERBOSITY terse`,
		"CREATE ROLE %I",
		"ALTER ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L",
		"GRANT pg_monitor, pg_use_reserved_connections TO %I",
		"ALTER DATABASE %I OWNER TO %I",
		"ELSIF v_kind = 'app' THEN",
		"IF v_db = 'postgres' THEN",
		"GRANT CONNECT ON DATABASE %I TO %I",
		"GRANT USAGE, CREATE ON SCHEMA public TO %I",
		"REVOKE pg_monitor, pg_use_reserved_connections FROM %I",
		"RAISE EXCEPTION 'unknown managed role kind %'",
		"v_marker TEXT := 'managed by the pg chart';",
		"COMMENT ON ROLE %I IS %L",
		"shobj_description((SELECT oid FROM pg_roles WHERE rolname = v_name), 'pg_authid') IS DISTINCT FROM v_marker",
		notManagedPhrase,
	} {
		if !strings.Contains(ensureRoleSQL, want) {
			t.Errorf("SQL missing %q", want)
		}
	}
	iLog := strings.Index(ensureRoleSQL, "SET log_statement = 'none';")
	iSecret := strings.Index(ensureRoleSQL, "myvars.role_secret = :'role_secret'")
	if iLog < 0 || iSecret < 0 || iLog > iSecret {
		t.Errorf("log suppression must precede the verifier-bearing SET")
	}
}

func TestEnsureRoleRejectsMissingInputs(t *testing.T) {
	ok := ManagedRole{Name: "app", Password: "pw", Kind: RoleApp}
	for name, c := range map[string]struct {
		super, db string
		r         ManagedRole
	}{
		"no superuser": {"", "db", ok},
		"no database":  {"postgres", "", ok},
		"no role":      {"postgres", "db", ManagedRole{Password: "pw", Kind: RoleApp}},
		"no password":  {"postgres", "db", ManagedRole{Name: "app", Kind: RoleApp}},
		"unknown kind": {"postgres", "db", ManagedRole{Name: "app", Password: "pw", Kind: "admin"}},
	} {
		f := &fakeStdinExec{}
		if err := EnsureRole(context.Background(), f, c.super, c.db, c.r); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if f.called {
			t.Errorf("%s: psql ran despite invalid input", name)
		}
	}
}

func TestEnsureRolePermanentAndTransientErrors(t *testing.T) {
	err := EnsureRole(context.Background(), &fakeStdinExec{}, "postgres", "db", ManagedRole{Name: "app", Password: "pässwörd ", Kind: RoleApp})
	if !errors.Is(err, ErrNeedsSASLprep) {
		t.Errorf("a password needing SASLprep must be a permanent error, got %v", err)
	}
	err = EnsureRole(context.Background(), &fakeStdinExec{err: errors.New("psql: connection refused")}, "postgres", "db", ManagedRole{Name: "app", Password: "pw", Kind: RoleApp})
	if err == nil || errors.Is(err, ErrNeedsSASLprep) || errors.Is(err, ErrRoleNotManaged) {
		t.Errorf("a psql failure must be a retryable error, got %v", err)
	}
}

// A same-named role the chart did not create is refused by the SQL; EnsureRole reports it
// as permanent so the agent logs it once instead of retrying every tick.
func TestEnsureRoleNotManagedIsPermanent(t *testing.T) {
	f := &fakeStdinExec{err: errors.New(`psql: exit status 3: ERROR:  role app_user already exists and is not managed by the pg chart: rename the chart value, or hand the role over with COMMENT ON ROLE app_user IS 'managed by the pg chart'`)}
	err := EnsureRole(context.Background(), f, "postgres", "db", ManagedRole{Name: "app_user", Password: "pw", Kind: RoleApp})
	if !errors.Is(err, ErrRoleNotManaged) {
		t.Errorf("want a permanent ErrRoleNotManaged, got %v", err)
	}
}
