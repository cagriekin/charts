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
// and passes the secret only through the environment, never on argv.
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
// 14). The username arrives as the psql var :'u'; the password via \getenv from
// REHASH_TGT_PASS (kept off argv). The values are hoisted into per-session GUCs and
// read back inside the DO block via current_setting() (a DO block cannot see psql
// vars directly), and format(%I,%L) quotes the identifier + literal safely. Idempotent:
// a no-op once the password is already scram. Ported verbatim from the chart's former
// postStart fix_user_auth (#199) so behavior is unchanged -- only the author moved.
const rehashSQL = `\getenv tgt_pass REHASH_TGT_PASS
SET myvars.tgt_user = :'u';
SET myvars.tgt_pass = :'tgt_pass';
DO $$
DECLARE
  v_user TEXT := current_setting('myvars.tgt_user');
  v_pass TEXT := current_setting('myvars.tgt_pass');
BEGIN
  IF current_setting('server_version_num')::int < 140000 THEN
    RAISE NOTICE 'Skipping md5->scram migration on PG < 14 (no md5->scram auto-promotion in pg_hba)';
    RETURN;
  END IF;
  IF EXISTS (
    SELECT 1 FROM pg_authid WHERE rolname = v_user AND rolpassword LIKE 'md5%'
  ) THEN
    SET LOCAL password_encryption = 'scram-sha-256';
    EXECUTE format('ALTER USER %I WITH PASSWORD %L', v_user, v_pass);
  END IF;
END
$$;`

// RehashMd5User re-hashes targetUser's password to scram-sha-256 if it is still stored
// as md5. It connects as superUser over the LOCAL socket (the agent's pg_hba
// `local all all trust` line -- no connection password), so it runs on the primary the
// agent is colocated with. The target password is passed via the environment
// (REHASH_TGT_PASS) and substituted with psql \getenv, so it never appears on argv.
// No-op when any required argument is empty; idempotent and safe on PG<14.
func RehashMd5User(ctx context.Context, ex StdinExec, superUser, db, targetUser, targetPass string) error {
	if superUser == "" || db == "" || targetUser == "" || targetPass == "" {
		return nil
	}
	return ex.RunStdin(ctx,
		[]string{"REHASH_TGT_PASS=" + targetPass},
		rehashSQL, "psql",
		"-U", superUser, "-d", db,
		"-v", "ON_ERROR_STOP=1",
		"-v", "u="+targetUser,
		"--no-psqlrc")
}
