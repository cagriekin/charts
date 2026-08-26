package pgconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initdbStockPreload is the line initdb itself writes into every data directory. It is
// commented out, and stripping it would turn PostgreSQL's own default into an explicit
// pin, so every test below asserts it survives verbatim.
const initdbStockPreload = "#shared_preload_libraries = ''\t# (change requires restart)"

// entrypointPreload is what images/repmgr/entrypoint.sh appends at initdb time -- the
// exact line #293 exists to remove.
const entrypointPreload = "shared_preload_libraries = 'repmgr'"

func TestStripPreloadLibraryDropsTheLineWhenRepmgrIsTheOnlyEntry(t *testing.T) {
	conf := "wal_level = replica\n" + initdbStockPreload + "\n" + entrypointPreload + "\nwal_log_hints = on\n"
	out, changed := stripPreloadLibrary(conf, "repmgr")
	if !changed {
		t.Fatal("expected a change")
	}
	// The whole assignment goes, rather than being left as an empty string: no setting and
	// an empty setting are equivalent, and dropping it lets PostgreSQL's default apply.
	if strings.Contains(out, entrypointPreload) {
		t.Errorf("the active assignment survived:\n%s", out)
	}
	if strings.Contains(out, "shared_preload_libraries = ''\n") {
		t.Errorf("left an empty assignment instead of dropping the line:\n%s", out)
	}
	if !strings.Contains(out, initdbStockPreload) {
		t.Errorf("initdb's commented-out default was touched:\n%s", out)
	}
	// #293's non-goals: the GUCs written by the same entrypoint block must not move.
	if !strings.Contains(out, "wal_level = replica") || !strings.Contains(out, "wal_log_hints = on") {
		t.Errorf("an unrelated setting was dropped:\n%s", out)
	}
}

func TestStripPreloadLibraryPreservesTheOtherLibraries(t *testing.T) {
	conf := "shared_preload_libraries = 'repmgr,pg_stat_statements,pgaudit'\n"
	out, changed := stripPreloadLibrary(conf, "repmgr")
	if !changed {
		t.Fatal("expected a change")
	}
	// Order matters: shared_preload_libraries is load order, not a set.
	if got := strings.TrimSpace(out); got != "shared_preload_libraries = 'pg_stat_statements,pgaudit'" {
		t.Errorf("neighbours or their order were not preserved: %q", got)
	}
}

func TestStripPreloadLibraryIsIdempotent(t *testing.T) {
	conf := "shared_preload_libraries = 'repmgr,pgaudit'\n"
	out, changed := stripPreloadLibrary(conf, "repmgr")
	if !changed {
		t.Fatal("expected a change on the first call")
	}
	out2, changed2 := stripPreloadLibrary(out, "repmgr")
	if changed2 {
		t.Errorf("second call must be a no-op, got a change:\n%s", out2)
	}
	if out2 != out {
		t.Errorf("second call must return identical content:\nfirst:  %q\nsecond: %q", out, out2)
	}
}

func TestStripPreloadLibraryRewritesEveryOccurrence(t *testing.T) {
	// PostgreSQL takes the LAST assignment, but stripping only that one would leave the
	// file's meaning dependent on which line a later editor happens to append after.
	conf := "shared_preload_libraries = 'repmgr'\nwal_level = replica\nshared_preload_libraries = 'repmgr,pgaudit'\n"
	out, changed := stripPreloadLibrary(conf, "repmgr")
	if !changed {
		t.Fatal("expected a change")
	}
	if strings.Contains(out, "'repmgr'") || strings.Contains(out, "'repmgr,") {
		t.Errorf("an earlier occurrence was left behind:\n%s", out)
	}
	if !strings.Contains(out, "shared_preload_libraries = 'pgaudit'") {
		t.Errorf("the surviving assignment lost its other libraries:\n%s", out)
	}
}

func TestStripPreloadLibraryHandlesQuotingAndTrailingText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// GUC names are case-insensitive in PostgreSQL, so a data directory carrying a
		// differently-cased key is the same setting and must still be cleaned.
		{"mixed-case key", "Shared_Preload_Libraries = 'repmgr,pgaudit'\n", "shared_preload_libraries = 'pgaudit'"},
		{"no spaces around =", "shared_preload_libraries='repmgr,pgaudit'\n", "shared_preload_libraries = 'pgaudit'"},
		{"spaces inside the list", "shared_preload_libraries = 'repmgr, pgaudit'\n", "shared_preload_libraries = 'pgaudit'"},
		{"trailing comment kept", "shared_preload_libraries = 'repmgr,pgaudit' # why\n", "shared_preload_libraries = 'pgaudit' # why"},
		{"leading indentation kept", "  shared_preload_libraries = 'repmgr,pgaudit'\n", "  shared_preload_libraries = 'pgaudit'"},
		{"bare unquoted token", "shared_preload_libraries = repmgr\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed := stripPreloadLibrary(c.in, "repmgr")
			if !changed {
				t.Fatalf("expected a change for %q", c.in)
			}
			if got := strings.TrimRight(out, "\n"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripPreloadLibraryLeavesUnrelatedContentAlone(t *testing.T) {
	cases := []string{
		"",
		"wal_level = replica\n",
		initdbStockPreload + "\n",
		"shared_preload_libraries = 'pgaudit'\n",
		// A case-differing library name is a DIFFERENT file (repmgr -> repmgr.so), so
		// folding case here could strip something unrelated.
		"shared_preload_libraries = 'Repmgr'\n",
		// An empty list has nothing to remove.
		"shared_preload_libraries = ''\n",
		// An unterminated quote is a syntax error the operator can see; guessing at it
		// would silently turn it into a different setting.
		"shared_preload_libraries = 'repmgr\n",
		// A library whose name merely contains repmgr is not repmgr.
		"shared_preload_libraries = 'repmgr_extra'\n",
	}
	for _, in := range cases {
		out, changed := stripPreloadLibrary(in, "repmgr")
		if changed {
			t.Errorf("unexpected change for %q:\n%s", in, out)
		}
		if out != in {
			t.Errorf("content was rewritten for %q: got %q", in, out)
		}
	}
}

func TestPreloadRequests(t *testing.T) {
	yes := []string{
		entrypointPreload + "\n",
		"shared_preload_libraries = 'pgaudit,repmgr'\n",
		"shared_preload_libraries = repmgr\n",
	}
	for _, in := range yes {
		if !preloadRequests(in, "repmgr") {
			t.Errorf("expected repmgr to be reported as requested in %q", in)
		}
	}
	no := []string{
		"",
		initdbStockPreload + "\n",
		"shared_preload_libraries = 'pgaudit'\n",
		"shared_preload_libraries = 'repmgr_extra'\n",
	}
	for _, in := range no {
		if preloadRequests(in, "repmgr") {
			t.Errorf("did not expect repmgr to be reported as requested in %q", in)
		}
	}
}

func TestEnsureNoPreloadLibraryWritesAtomicallyAndConverges(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "postgresql.conf")
	if err := os.WriteFile(conf, []byte("wal_level = replica\n"+entrypointPreload+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := EnsureNoPreloadLibrary(conf, "repmgr")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected a change")
	}
	got, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "repmgr") {
		t.Errorf("repmgr survived on disk:\n%s", got)
	}
	if !strings.Contains(string(got), "wal_level = replica") {
		t.Errorf("an unrelated setting was lost:\n%s", got)
	}
	// The mode must stay 0600: this file lives in PGDATA, which PostgreSQL refuses to
	// start on if it is group- or world-readable.
	fi, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %v, want 0600", perm)
	}
	// No temp file may be left behind in PGDATA -- the postmaster scans this directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only postgresql.conf to remain, got %d entries", len(entries))
	}
	// Converged: a second call neither errors nor reports a change.
	changed2, err := EnsureNoPreloadLibrary(conf, "repmgr")
	if err != nil {
		t.Fatal(err)
	}
	if changed2 {
		t.Error("second call reported a change on an already-clean file")
	}
}

func TestEnsureNoPreloadLibraryErrorsOnAMissingFile(t *testing.T) {
	// PGDATA/postgresql.conf always exists once the directory is initialized, so its
	// absence is a real problem and must not be swallowed.
	if _, err := EnsureNoPreloadLibrary(filepath.Join(t.TempDir(), "nope.conf"), "repmgr"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestPreloadsLibraryTreatsAMissingFileAsAbsent(t *testing.T) {
	// Unlike postgresql.conf, the paths this predicate scans (postgresql.auto.conf, each
	// conf.d fragment) legitimately may not exist.
	got, err := PreloadsLibrary(filepath.Join(t.TempDir(), "nope.conf"), "repmgr")
	if err != nil {
		t.Fatalf("a missing file must not be an error: %v", err)
	}
	if got {
		t.Error("a missing file must not report the library as requested")
	}
}
