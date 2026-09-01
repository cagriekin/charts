package pgconf

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/cagriekin/pg-ha-agent/internal/atomicfile"
)

// conninfoPasswordRe matches one libpq `password=` keyword and its value, quoted or bare.
// The quoted alternative comes first so a password containing whitespace is consumed whole
// rather than truncated at the first space.
var conninfoPasswordRe = regexp.MustCompile(`(?i)\bpassword[ \t]*=[ \t]*('(?:[^']|'')*'?|[^ \t]*)`)

// RedactConninfo replaces any password in a libpq conninfo with a marker, so the value can be
// logged (#298 review).
//
// The agent's OWN conninfo is passwordless by construction -- it authenticates through the
// 0600 ~/.pgpass writePgpass lays down -- but the values this package reads out of
// postgresql.auto.conf are not the agent's: they come from repmgr's clone/follow, or from an
// operator's manual `ALTER SYSTEM SET primary_conninfo`, and libpq accepts `password=` there.
// Logging one verbatim would put a replication credential in the pod log and in every log
// sink downstream of it, which is the exact class of leak the repo's code-scanning alert was
// filed for. Redacting costs one regexp and keeps the rest of the string -- host, port, user,
// application_name -- which is all the diagnostic value the log line ever had.
func RedactConninfo(conninfo string) string {
	return conninfoPasswordRe.ReplaceAllString(conninfo, "password=[redacted]")
}

// EnsurePrimaryConninfoDBName reads confPath (PGDATA/postgresql.auto.conf, where repmgr's
// own `standby clone`/`standby follow` CLI writes primary_conninfo), appends
// dbname=<db> to it if not already present, and writes the file back. Returns
// changed=false, nil when there is no primary_conninfo line or dbname is already set, so
// this is always safe to call unconditionally after any mechanism operation that might
// have (re)written primary_conninfo -- callers reload the postmaster only when changed.
func EnsurePrimaryConninfoDBName(confPath, db string) (changed bool, err error) {
	data, err := os.ReadFile(confPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", confPath, err)
	}
	updated, changed, malformed := addDBNameToPrimaryConninfo(string(data), db)
	if !changed {
		if malformed {
			return false, fmt.Errorf("%s: primary_conninfo line does not match the expected 'key=''value'' ...' shape -- left untouched", confPath)
		}
		return false, nil
	}
	if err := atomicfile.Write(confPath, []byte(updated), 0o600); err != nil {
		return false, fmt.Errorf("write %s: %w", confPath, err)
	}
	return true, nil
}

// addDBNameToPrimaryConninfo is the pure transform behind EnsurePrimaryConninfoDBName.
//
// primary_conninfo is written by repmgr's own clone/follow CLI, never by this codebase, as
// a postgresql.conf-quoted string (outer '...', embedded single quotes doubled --
// PostgreSQL's own string-escaping convention for a GUC value) wrapping a raw libpq
// keyword=value conninfo string that itself single-quotes individual values, e.g.:
//
//	primary_conninfo = 'host=''pg-0.h'' port=5432 user=repmgr application_name=''pg-1'' connect_timeout=10'
//
// It never includes dbname -- physical replication doesn't need it, but PostgreSQL 17+'s
// sync_replication_slots worker does (#308). This appends dbname='<db>' to the raw conninfo
// and re-escapes, touching only the primary_conninfo line; every other line (including any
// GUC set via ALTER SYSTEM, e.g. #308's synchronized_standby_slots on a primary) is passed
// through byte-for-byte.
//
// db is assumed not to contain a single quote (chart-declared database/role identifiers do
// not; this matches the escaping model already used for repmgr.conf's own conninfo, which
// makes the same assumption for host/user).
// Returns malformed=true only when a primary_conninfo line exists but does not match
// the expected quoted shape -- distinct from the two other, benign not-changed cases
// (no primary_conninfo line at all, e.g. on a primary; dbname already present) so a
// caller can specifically warn about the one case that means this parser doesn't
// recognize a line repmgr actually wrote, rather than treating all three alike.
func addDBNameToPrimaryConninfo(conf, db string) (updated string, changed bool, malformed bool) {
	lines := strings.Split(conf, "\n")
	// PostgreSQL parses postgresql.auto.conf top-to-bottom and the LAST occurrence of a
	// GUC wins -- a duplicate primary_conninfo line is not something repmgr writes today,
	// but patching the first match while PostgreSQL honors the last would be a silent
	// no-op (changed=true triggers a reload, but the live value never picks up dbname).
	last := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "primary_conninfo") {
			last = i
		}
	}
	if last < 0 {
		return conf, false, false
	}
	newLine, ok, lineMalformed := addDBNameToLine(lines[last], db)
	if !ok {
		return conf, false, lineMalformed
	}
	lines[last] = newLine
	return strings.Join(lines, "\n"), true, false
}

// hasDBNameKeyword reports whether raw's libpq keyword=value conninfo already has a
// dbname keyword, checked token-by-token (whitespace-delimited, libpq's own conninfo
// separator) rather than a bare substring match -- a value that happens to contain the
// literal text "dbname=" (e.g. inside an options='...' value) does not fool it the way
// strings.Contains would.
func hasDBNameKeyword(raw string) bool {
	for _, tok := range strings.Fields(raw) {
		if strings.HasPrefix(tok, "dbname=") {
			return true
		}
	}
	return false
}

// addDBNameToLine handles one `primary_conninfo = '...'` line. Returns ok=false with
// malformed=true when the line has no matching quote pair (leave it untouched rather
// than guess -- distinct from ok=false, malformed=false, which means dbname is already
// present, an entirely benign no-op).
func addDBNameToLine(line, db string) (updated string, ok bool, malformed bool) {
	i := strings.Index(line, "'")
	j := strings.LastIndex(line, "'")
	if i < 0 || j <= i {
		return line, false, true
	}
	prefix, quoted, suffix := line[:i+1], line[i+1:j], line[j:]
	raw := strings.ReplaceAll(quoted, "''", "'") // undo postgresql.conf's outer doubling
	// An EMPTY value is a setting, not a gap to fill (#298 review). `primary_conninfo = ''`
	// means "do not stream" -- the same meaning RemoveRecoveryConfig's own comment relies on
	// when it refuses to blank these GUCs ("an empty string is a valid setting meaning 'do not
	// stream', so blanking them would leave a standby that never connects"). Appending dbname
	// to it produced `primary_conninfo = ' dbname=''repmgr'''`: a NON-empty conninfo with no
	// host, port or user, so libpq falls back to the local unix socket and the walreceiver
	// dials its own postmaster -- and the Follow branch reloads it, because changed=true.
	// Reachable after boot (RemoveRecoveryConfig is a one-time preflight): an operator who
	// pauses replication with `ALTER SYSTEM SET primary_conninfo = ''` had it silently
	// un-paused into a self-connect loop on the next tick, permanently -- hasDBNameKeyword
	// then matched, so the line was never revisited.
	if strings.TrimSpace(raw) == "" {
		return line, false, false
	}
	if hasDBNameKeyword(raw) {
		return line, false, false
	}
	raw += fmt.Sprintf(" dbname='%s'", db)
	requoted := strings.ReplaceAll(raw, "'", "''") // redo postgresql.conf's outer doubling
	return prefix + requoted + suffix, true, false
}
