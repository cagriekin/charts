package pgconf

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// preloadGUC is the parameter this file edits. PostgreSQL GUC names are
// case-insensitive, so the line matcher below is too -- a data directory carrying
// `Shared_Preload_Libraries` is the same setting and must not be skipped.
const preloadGUC = "shared_preload_libraries"

// preloadAssignRe matches an ACTIVE shared_preload_libraries assignment, capturing the
// leading indentation and everything after the `=`. Commented-out lines are excluded by
// the caller, not here, so that initdb's own stock
// `#shared_preload_libraries = ”	# (change requires restart)` is never touched.
var preloadAssignRe = regexp.MustCompile(`(?i)^([ \t]*)` + preloadGUC + `[ \t]*=[ \t]*(.*)$`)

// EnsureNoPreloadLibrary removes lib from every shared_preload_libraries assignment in
// confPath, preserving the other libraries and their order, and reports whether the file
// changed. When lib was the only entry the assignment line is dropped entirely rather than
// left as an empty string: an empty `shared_preload_libraries = ”` and no setting at all
// are equivalent to the postmaster, and dropping the line lets PostgreSQL's own default
// apply instead of pinning it.
//
// This exists because `shared_preload_libraries = 'repmgr'` is persisted INSIDE the data
// directory -- images/repmgr/entrypoint.sh appends it at initdb time, and every standby
// clones it verbatim -- so it survives any chart change and any helm rollback (#293). Once
// the repmgr package leaves the image (#290) a data directory still asking for repmgr.so
// is a postmaster that will not start, on every pod simultaneously. Removing it has to
// happen from inside the running node, a release BEFORE the image that makes it mandatory.
//
// EVERY occurrence is rewritten, not just the last one. PostgreSQL takes the last
// assignment, so stripping only that one would still leave the file's meaning dependent on
// which line a later editor happens to append after.
func EnsureNoPreloadLibrary(confPath, lib string) (changed bool, err error) {
	data, err := os.ReadFile(confPath)
	// A missing postgresql.conf is not this function's problem to escalate. It happens on a
	// torn clone or restore that left PG_VERSION behind (so the caller's HasData check
	// passes) without the config, and boot()'s later ReadControlData already handles that
	// gracefully -- it warns and defers the start to the reconcile loop. Erroring here would
	// abort boot() one step earlier and skip that recovery path entirely.
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", confPath, err)
	}
	updated, changed := stripPreloadLibrary(string(data), lib)
	if !changed {
		return false, nil
	}
	// ATOMIC, for the same reason EnsureConfdInclude is: this is PGDATA/postgresql.conf,
	// and a truncated or half-written copy is not a degraded config but a postmaster that
	// refuses to start at all, with no automatic recovery.
	if err := writeFileAtomic(confPath, []byte(updated), 0o600); err != nil {
		return false, fmt.Errorf("write %s: %w", confPath, err)
	}
	return true, nil
}

// PreloadsLibrary reports whether confPath has an active assignment listing lib. A missing
// file is not an error and reports false: callers scan several optional paths
// (postgresql.auto.conf, each conf.d fragment), any of which legitimately may not exist.
func PreloadsLibrary(confPath, lib string) (bool, error) {
	data, err := os.ReadFile(confPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", confPath, err)
	}
	return preloadRequests(string(data), lib), nil
}

// stripPreloadLibrary is the pure transform behind EnsureNoPreloadLibrary.
func stripPreloadLibrary(content, lib string) (string, bool) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	changed := false
	for _, ln := range lines {
		indent, libs, trailing, ok := parsePreloadAssignment(ln)
		if !ok {
			out = append(out, ln)
			continue
		}
		kept := filterOutLib(libs, lib)
		if len(kept) == len(libs) {
			out = append(out, ln)
			continue
		}
		changed = true
		if len(kept) == 0 {
			// Drop the line. Any trailing comment went with the setting it annotated.
			continue
		}
		out = append(out, indent+preloadGUC+" = '"+strings.ReplaceAll(strings.Join(kept, ","), "'", "''")+"'"+trailing)
	}
	if !changed {
		return content, false
	}
	return strings.Join(out, "\n"), true
}

// preloadRequests reports whether any active assignment in content lists lib.
func preloadRequests(content, lib string) bool {
	for _, ln := range strings.Split(content, "\n") {
		_, libs, _, ok := parsePreloadAssignment(ln)
		if !ok {
			continue
		}
		if len(filterOutLib(libs, lib)) != len(libs) {
			return true
		}
	}
	return false
}

// filterOutLib returns libs without lib.
//
// An entry may be directory-qualified and may carry the shared-object suffix -- PostgreSQL
// accepts `repmgr`, `$libdir/repmgr`, `repmgr.so` and `/usr/lib/postgresql/18/lib/repmgr.so`
// as the same library, adding `$libdir/` and `.so` itself when they are absent. Matching the
// bare name only would let `$libdir/repmgr` slip past every layer of #293 at once: the strip
// would leave it, the diagnostic would stay silent about it, and the render guard would
// accept it -- delivering exactly the bare `could not access file "repmgr"` crash-loop all
// three exist to prevent.
//
// Case is NOT folded. These entries resolve as filenames, so unlike the GUC name they are
// case-sensitive wherever the filesystem is, and folding could strip an unrelated library
// that merely differs in case. Non-matching entries are kept verbatim, never normalised.
func filterOutLib(libs []string, lib string) []string {
	kept := make([]string, 0, len(libs))
	for _, l := range libs {
		if preloadEntryNames(l, lib) {
			continue
		}
		kept = append(kept, l)
	}
	return kept
}

// preloadEntryNames reports whether a shared_preload_libraries entry refers to lib,
// tolerating a directory prefix and a .so suffix. Kept in one place so the strip and the
// read-only predicate cannot drift apart.
func preloadEntryNames(entry, lib string) bool {
	e := strings.TrimSpace(entry)
	if e == "" {
		return false
	}
	// path.Base collapses `$libdir/repmgr` and any absolute path to the bare name; ToSlash
	// first so a backslash-separated value cannot hide one.
	e = path.Base(filepath.ToSlash(e))
	return e == lib || e == lib+".so"
}

// parsePreloadAssignment splits one postgresql.conf line into its indentation, the library
// list, and whatever followed the value (a trailing comment, usually). ok is false for any
// line that is not an active shared_preload_libraries assignment -- including a
// commented-out one, which must survive verbatim: initdb writes
// `#shared_preload_libraries = ”` into every data directory, and "fixing" that would turn
// a stock default into an explicit pin.
//
// The value is either a single-quoted string (embedded quotes doubled, per
// postgresql.conf's own quoting) or, for a single token, bare.
func parsePreloadAssignment(line string) (indent string, libs []string, trailing string, ok bool) {
	if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
		return "", nil, "", false
	}
	m := preloadAssignRe.FindStringSubmatch(line)
	if m == nil {
		return "", nil, "", false
	}
	raw, rest := splitPreloadValue(m[2])
	// An assignment whose value we cannot parse (an unterminated quote) is left alone
	// rather than guessed at: rewriting it would risk turning a syntax error the operator
	// can see into a silently different setting.
	if raw == "" && rest == "" {
		return "", nil, "", false
	}
	for _, part := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(part); t != "" {
			libs = append(libs, t)
		}
	}
	return m[1], libs, rest, true
}

// splitPreloadValue separates the value from any trailing text on the line. It returns
// ("", "") when the value is a quoted string with no closing quote, which the caller reads
// as "unparseable, leave the line alone".
func splitPreloadValue(s string) (value, trailing string) {
	s = strings.TrimLeft(s, " \t")
	if !strings.HasPrefix(s, "'") {
		// Bare token: ends at whitespace or the start of a comment.
		end := strings.IndexAny(s, " \t#")
		if end < 0 {
			return s, ""
		}
		return s[:end], s[end:]
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		// A doubled '' is one literal quote inside the string; a single one ends it.
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), s[i+1:]
	}
	return "", ""
}
