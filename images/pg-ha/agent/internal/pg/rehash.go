package pg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// StdinExec runs an external command with data piped to stdin. The md5->scram
// re-hash feeds its SQL via stdin (psql MainLoop, where :'var' substitution works)
// and passes the SCRAM verifier only through the environment, never on argv.
type StdinExec interface {
	RunStdin(ctx context.Context, env []string, stdin, name string, args ...string) error
}

// RunStdin executes name with args and stdin piped in, appending env to the current
// environment. Combined output is folded into the error for diagnostics.
func (OSExec) RunStdin(ctx context.Context, env []string, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// rehashSQL re-hashes one managed user's password from md5 to scram-sha-256 when it
// is still stored as md5 (PG14+ only -- md5->scram pg_hba auto-promotion exists from
// 14). Idempotent: a no-op once the password is already scram.
//
// What crosses the wire is a SCRAM VERIFIER, never the plaintext (#298 review). The
// previous form sent the password as a SQL literal via `SET myvars.tgt_pass = :'tgt_pass'`,
// which kept it off argv but not out of the SERVER LOG: a top-level SET is logged verbatim
// under log_statement = 'all' or log_min_duration_statement = 0, so any cluster with
// statement logging on recorded the superuser and replication passwords in cleartext and
// then shipped them wherever its logs go. PostgreSQL stores a well-formed
// `SCRAM-SHA-256$...` string in ALTER USER ... PASSWORD verbatim rather than re-hashing it,
// so hashing in Go (see ScramVerifier) produces exactly the secret the server would have
// produced itself.
//
// The log-suppression SETs come FIRST, before anything carrying the verifier, and they are
// defence in depth rather than the fix: a verifier still permits an offline dictionary
// attack, and password_encryption is not the only GUC an operator can change under us.
// Ordering matters -- a SET issued before log_statement is lowered is logged under the old
// value, so the sequence here is not cosmetic.
//
// The username arrives as the psql var :'u'; the verifier via \getenv from
// REHASH_TGT_SECRET (kept off argv). Both are hoisted into per-session GUCs and read back
// inside the DO block via current_setting() (a DO block cannot see psql vars directly), and
// format(%I,%L) quotes the identifier + literal safely. Derived from the chart's former
// postStart fix_user_auth (#199); the md5 gate and idempotence are unchanged.
const rehashSQL = `\getenv tgt_secret REHASH_TGT_SECRET
SET log_statement = 'none';
SET log_min_duration_statement = -1;
SET log_min_error_statement = 'panic';
SET myvars.tgt_user = :'u';
SET myvars.tgt_secret = :'tgt_secret';
DO $$
DECLARE
  v_user TEXT := current_setting('myvars.tgt_user');
  v_secret TEXT := current_setting('myvars.tgt_secret');
BEGIN
  IF current_setting('server_version_num')::int < 140000 THEN
    RAISE NOTICE 'Skipping md5->scram migration on PG < 14 (no md5->scram auto-promotion in pg_hba)';
    RETURN;
  END IF;
  IF EXISTS (
    SELECT 1 FROM pg_authid WHERE rolname = v_user AND rolpassword LIKE 'md5%'
  ) THEN
    -- v_secret is already a SCRAM-SHA-256 verifier, which PostgreSQL stores verbatim
    -- without consulting the encryption GUC, so this path sets no encryption at all.
    EXECUTE format('ALTER USER %I WITH PASSWORD %L', v_user, v_secret);
  END IF;
END
$$;`

// RehashMd5User re-hashes targetUser's password to scram-sha-256 if it is still stored
// as md5. It connects as superUser over the LOCAL socket (the agent's pg_hba
// `local all all trust` line -- no connection password), so it runs on the primary the
// agent is colocated with. The plaintext is hashed HERE and never leaves this process:
// only the SCRAM verifier is sent, and only through the environment (REHASH_TGT_SECRET),
// so it appears neither on argv nor -- under statement logging -- in the server log.
// No-op when any required argument is empty; idempotent and safe on PG<14.
func RehashMd5User(ctx context.Context, ex StdinExec, superUser, db, targetUser, targetPass string) error {
	if superUser == "" || db == "" || targetUser == "" || targetPass == "" {
		return nil
	}
	secret, err := ScramVerifier(targetPass)
	if err != nil {
		// ErrNeedsSASLprep is passed through UNWRAPPED-but-matchable so the caller can tell a
		// permanent skip from a transient failure; see rehashManagedUsersOnce.
		return fmt.Errorf("rehash %s: %w", targetUser, err)
	}
	return ex.RunStdin(ctx,
		[]string{"REHASH_TGT_SECRET=" + secret},
		rehashSQL, "psql",
		"-U", superUser, "-d", db,
		"-v", "ON_ERROR_STOP=1",
		"-v", "u="+targetUser,
		"--no-psqlrc")
}
