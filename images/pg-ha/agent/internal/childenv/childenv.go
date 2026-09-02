// Package childenv builds the environment handed to the agent's child processes
// (psql, pg_basebackup, pg_rewind, pg_ctl, pgbackrest). It strips the agent's own
// credential variables -- REPMGR_PASSWORD, POSTGRES_PASSWORD, and anything else
// whose name carries "PASSWORD" -- from the inherited environment (#298 security
// review). Those children authenticate through PGPASSWORD/PGPASSFILE supplied
// explicitly by the caller and never read the raw agent secrets, so a verbose or
// compromised child (pgBackRest in particular parses every PGBACKREST_* variable)
// should not find the cluster's passwords sitting in its /proc/<pid>/environ.
//
// PGPASSWORD is deliberately NOT stripped: a caller may pass it through the extra
// slice, which is appended last and so always wins.
package childenv

import "strings"

// Filtered returns base with credential variables removed, then extra appended.
// base is normally os.Environ(); extra carries the per-call additions (e.g.
// PGPASSWORD) and takes precedence over anything left in base.
func Filtered(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if name != "PGPASSWORD" && strings.Contains(name, "PASSWORD") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}
