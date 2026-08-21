package pgconf

import (
	"fmt"
	"os"
	"strings"
)

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
	updated, changed := addDBNameToPrimaryConninfo(string(data), db)
	if !changed {
		return false, nil
	}
	if err := os.WriteFile(confPath, []byte(updated), 0o600); err != nil {
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
func addDBNameToPrimaryConninfo(conf, db string) (string, bool) {
	lines := strings.Split(conf, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(trimmed), "primary_conninfo") {
			continue
		}
		newLine, ok := addDBNameToLine(line, db)
		if !ok {
			return conf, false
		}
		lines[i] = newLine
		return strings.Join(lines, "\n"), true
	}
	return conf, false
}

// addDBNameToLine handles one `primary_conninfo = '...'` line. Returns ok=false
// unchanged when the line has no matching quote pair (malformed -- leave it untouched
// rather than guess) or dbname is already present.
func addDBNameToLine(line, db string) (string, bool) {
	i := strings.Index(line, "'")
	j := strings.LastIndex(line, "'")
	if i < 0 || j <= i {
		return line, false
	}
	prefix, quoted, suffix := line[:i+1], line[i+1:j], line[j:]
	raw := strings.ReplaceAll(quoted, "''", "'") // undo postgresql.conf's outer doubling
	if strings.Contains(raw, "dbname=") {
		return line, false
	}
	raw += fmt.Sprintf(" dbname='%s'", db)
	requoted := strings.ReplaceAll(raw, "'", "''") // redo postgresql.conf's outer doubling
	return prefix + requoted + suffix, true
}
