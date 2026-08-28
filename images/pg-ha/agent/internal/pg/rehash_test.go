package pg

import (
	"context"
	"strings"
	"testing"
)

type fakeStdinExec struct {
	called bool
	env    []string
	stdin  string
	name   string
	args   []string
	err    error
}

func (f *fakeStdinExec) RunStdin(ctx context.Context, env []string, stdin, name string, args ...string) error {
	f.called = true
	f.env, f.stdin, f.name, f.args = env, stdin, name, args
	return f.err
}

func TestRehashMd5UserInvocation(t *testing.T) {
	f := &fakeStdinExec{}
	const pw = "s3cr3t'pw"
	if err := RehashMd5User(context.Background(), f, "postgres", "appdb", "medusa", pw); err != nil {
		t.Fatal(err)
	}
	if !f.called || f.name != "psql" {
		t.Fatalf("expected a psql call, got called=%v name=%q", f.called, f.name)
	}
	argline := strings.Join(f.args, " ")
	for _, want := range []string{"-U postgres", "-d appdb", "ON_ERROR_STOP=1", "u=medusa", "--no-psqlrc"} {
		if !strings.Contains(argline, want) {
			t.Errorf("args missing %q: %v", want, f.args)
		}
	}
	// The plaintext must not appear on argv, in the environment, or in the SQL: what travels
	// is a SCRAM verifier, and only via the environment (#298 review).
	if strings.Contains(argline, "s3cr3t") {
		t.Errorf("password leaked onto argv: %v", f.args)
	}
	if strings.Contains(f.stdin, "s3cr3t") {
		t.Errorf("password leaked into the SQL, where statement logging would capture it:\n%s", f.stdin)
	}
	secret := ""
	for _, e := range f.env {
		if v, ok := strings.CutPrefix(e, "REHASH_TGT_SECRET="); ok {
			secret = v
		}
		if strings.Contains(e, "s3cr3t") {
			t.Errorf("plaintext password passed in the environment: %v", e)
		}
	}
	if !strings.HasPrefix(secret, "SCRAM-SHA-256$4096:") {
		t.Errorf("expected a SCRAM verifier in REHASH_TGT_SECRET, got %q (env %v)", secret, f.env)
	}
	// The SQL guards PG<14 and re-hashes only md5-stored passwords, via \getenv + format.
	for _, want := range []string{
		`\getenv tgt_secret REHASH_TGT_SECRET`,
		"server_version_num",
		"rolpassword LIKE 'md5%'",
		"ALTER USER %I WITH PASSWORD %L",
	} {
		if !strings.Contains(f.stdin, want) {
			t.Errorf("SQL missing %q:\n%s", want, f.stdin)
		}
	}
	// password_encryption is no longer set: a verifier is stored verbatim, so consulting it
	// would be misleading rather than merely redundant.
	if strings.Contains(f.stdin, "password_encryption") {
		t.Errorf("password_encryption has no role once a verifier is sent:\n%s", f.stdin)
	}
	// Statement logging must be lowered BEFORE anything carrying the verifier is sent --
	// a SET issued first would be logged under the operator's original log_statement.
	iLog := strings.Index(f.stdin, "SET log_statement = 'none';")
	iSecret := strings.Index(f.stdin, "myvars.tgt_secret = :'tgt_secret'")
	if iLog < 0 || iSecret < 0 || iLog > iSecret {
		t.Errorf("log suppression must precede the verifier-bearing SET (log at %d, secret at %d):\n%s", iLog, iSecret, f.stdin)
	}
}

func TestRehashMd5UserSkipsEmptyArgs(t *testing.T) {
	cases := [][4]string{
		{"", "db", "u", "p"},
		{"super", "", "u", "p"},
		{"super", "db", "", "p"},
		{"super", "db", "u", ""},
	}
	for _, c := range cases {
		f := &fakeStdinExec{}
		if err := RehashMd5User(context.Background(), f, c[0], c[1], c[2], c[3]); err != nil {
			t.Fatal(err)
		}
		if f.called {
			t.Errorf("expected no-op for empty arg set %v, but RunStdin was called", c)
		}
	}
}
