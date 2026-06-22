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
	// The password must travel in the environment, NEVER on argv.
	if strings.Contains(argline, "s3cr3t") {
		t.Errorf("password leaked onto argv: %v", f.args)
	}
	found := false
	for _, e := range f.env {
		if e == "REHASH_TGT_PASS="+pw {
			found = true
		}
	}
	if !found {
		t.Errorf("password not passed via REHASH_TGT_PASS env: %v", f.env)
	}
	// The SQL guards PG<14 and re-hashes only md5-stored passwords, via \getenv + format.
	for _, want := range []string{
		`\getenv tgt_pass REHASH_TGT_PASS`,
		"server_version_num",
		"rolpassword LIKE 'md5%'",
		"password_encryption = 'scram-sha-256'",
		"ALTER USER %I WITH PASSWORD %L",
	} {
		if !strings.Contains(f.stdin, want) {
			t.Errorf("SQL missing %q:\n%s", want, f.stdin)
		}
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
