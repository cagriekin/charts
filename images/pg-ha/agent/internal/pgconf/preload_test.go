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

// entrypointPreload is what images/pg-ha/entrypoint.sh appends at initdb time -- the
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

func TestEnsureNoPreloadLibraryToleratesAMissingFile(t *testing.T) {
	// A torn clone or restore can leave PG_VERSION behind (so the caller's HasData check
	// passes) without postgresql.conf. Erroring here would abort boot() one step before
	// ReadControlData, which already handles that case gracefully by deferring the start to
	// the reconcile loop -- so escalating would REMOVE a recovery path, not add safety.
	changed, err := EnsureNoPreloadLibrary(filepath.Join(t.TempDir(), "nope.conf"), "repmgr")
	if err != nil {
		t.Fatalf("a missing postgresql.conf must not be an error: %v", err)
	}
	if changed {
		t.Error("a missing file cannot have changed")
	}
}

func TestPreloadEntryIsMatchedWhenDirectoryQualifiedOrSuffixed(t *testing.T) {
	// PostgreSQL accepts all of these as the same library, adding `$libdir/` and `.so`
	// itself when absent. Matching only the bare name let `$libdir/repmgr` slip past the
	// strip, the diagnostic AND the render guard simultaneously -- delivering exactly the
	// crash-loop all three exist to prevent (#293 review).
	for _, entry := range []string{
		"repmgr",
		"repmgr.so",
		"$libdir/repmgr",
		"$libdir/repmgr.so",
		"/usr/lib/postgresql/18/lib/repmgr.so",
		" $libdir/repmgr ",
	} {
		in := "shared_preload_libraries = '" + entry + ",pgaudit'\n"
		out, changed := stripPreloadLibrary(in, "repmgr")
		if !changed {
			t.Errorf("entry %q was not recognised as repmgr: %q", entry, out)
			continue
		}
		if got := strings.TrimSpace(out); got != "shared_preload_libraries = 'pgaudit'" {
			t.Errorf("entry %q: got %q", entry, got)
		}
		if !preloadRequests(in, "repmgr") {
			t.Errorf("entry %q was not reported as requesting repmgr", entry)
		}
	}
	// ...and a library that merely ends in the same characters is NOT repmgr.
	for _, entry := range []string{"my_repmgr", "$libdir/repmgr_extra", "repmgrd"} {
		in := "shared_preload_libraries = '" + entry + "'\n"
		if _, changed := stripPreloadLibrary(in, "repmgr"); changed {
			t.Errorf("entry %q must not be treated as repmgr", entry)
		}
		if preloadRequests(in, "repmgr") {
			t.Errorf("entry %q must not be reported as requesting repmgr", entry)
		}
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

func TestPreloadHandlesTheSpellingsPostgresAcceptsButWeAlmostMissed(t *testing.T) {
	// Both forms below were verified against a real postmaster in this image: it starts and
	// SHOW reports the library as loaded. Missing them meant the strip, the diagnostic and
	// the render guard all passed the value straight through to a crash-loop (#293 review).
	cases := []struct{ name, in, want string }{
		// SplitDirectoriesString strips double quotes around an entry.
		{"double-quoted entry", `shared_preload_libraries = '"repmgr"'` + "\n", ""},
		{"double-quoted among others", `shared_preload_libraries = '"repmgr",pgaudit'` + "\n", "shared_preload_libraries = 'pgaudit'"},
		// postgresql.conf makes the equals sign optional.
		{"no equals sign", "shared_preload_libraries 'repmgr'\n", ""},
		{"no equals sign among others", "shared_preload_libraries 'repmgr,pgaudit'\n", "shared_preload_libraries = 'pgaudit'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed := stripPreloadLibrary(c.in, "repmgr")
			if !changed {
				t.Fatalf("not recognised: %q", c.in)
			}
			if got := strings.TrimRight(out, "\n"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if !preloadRequests(c.in, "repmgr") {
				t.Errorf("not reported as requesting repmgr: %q", c.in)
			}
		})
	}
}

func TestPreloadAssignmentDoesNotMatchALongerParameterName(t *testing.T) {
	// Making the equals sign optional risks swallowing any parameter that merely starts with
	// this name, so one of {whitespace, =} is required after it.
	for _, in := range []string{
		"shared_preload_libraries_foo = 'repmgr'\n",
		"shared_preload_libraries2 = 'repmgr'\n",
	} {
		if _, changed := stripPreloadLibrary(in, "repmgr"); changed {
			t.Errorf("a different parameter was rewritten: %q", in)
		}
		if preloadRequests(in, "repmgr") {
			t.Errorf("a different parameter was read as the preload list: %q", in)
		}
	}
}

func TestSetsDynamicLibraryPath(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "c.conf")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, body := range []string{
		"dynamic_library_path = '$libdir:/opt/pg/lib'\n",
		"dynamic_library_path '$libdir'\n",
		"Dynamic_Library_Path = '$libdir'\n",
	} {
		got, err := SetsDynamicLibraryPath(write(body))
		if err != nil {
			t.Fatal(err)
		}
		if !got {
			t.Errorf("expected detection for %q", body)
		}
	}
	for _, body := range []string{
		"",
		"# dynamic_library_path = '$libdir'\n",
		"wal_level = replica\n",
		"dynamic_library_path_extra = '/x'\n",
	} {
		got, err := SetsDynamicLibraryPath(write(body))
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Errorf("unexpected detection for %q", body)
		}
	}
	if got, err := SetsDynamicLibraryPath(filepath.Join(dir, "absent.conf")); err != nil || got {
		t.Errorf("a missing file must be (false, nil), got (%v, %v)", got, err)
	}
}

func TestForeignRecoveryConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "postgresql.auto.conf")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// What repmgr actually left in a 1.x standby's auto.conf (#294 review). PostgreSQL reads
	// auto.conf after every include, so these outrank the agent's own fragment.
	const repmgrShaped = "# Do not edit this file manually!\n" +
		"primary_conninfo = 'host=''pg-0.h'' port=5432 user=repmgr'\n" +
		"primary_slot_name = 'repmgr_slot_1001'\n"
	got, err := ForeignRecoveryConfig(write(repmgrShaped))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want both recovery GUCs reported", got)
	}

	// Either one alone is enough to defeat the agent.
	for _, body := range []string{
		"primary_conninfo = 'host=x'\n",
		"primary_slot_name = 'pg_ha_slot_1'\n",
		"Primary_Conninfo = 'host=x'\n", // GUC names are case-insensitive
		"primary_slot_name 'x'\n",       // the equals sign is optional
	} {
		got, err := ForeignRecoveryConfig(write(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Errorf("not detected: %q", body)
		}
	}

	// What a correctly-operating native cluster's auto.conf holds: only the agent's own
	// ALTER SYSTEM (#308). That must not trip the refusal.
	for _, body := range []string{
		"",
		"# Do not edit this file manually!\n",
		"synchronized_standby_slots = 'pg_ha_slot_1,pg_ha_slot_2'\n",
		"#primary_conninfo = 'host=x'\n",
		"primary_conninfo_extra = 'x'\n",
	} {
		got, err := ForeignRecoveryConfig(write(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("false positive on %q: %v", body, got)
		}
	}

	// A fresh install has no auto.conf at all until the first ALTER SYSTEM.
	got, err = ForeignRecoveryConfig(filepath.Join(dir, "absent.conf"))
	if err != nil || len(got) != 0 {
		t.Errorf("a missing auto.conf must be (nil, nil), got (%v, %v)", got, err)
	}
}

func TestRemoveRecoveryConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "postgresql.auto.conf")
	// What repmgr's `standby follow` actually left behind, plus an ALTER SYSTEM the agent itself
	// makes (#308) that must survive.
	body := "# Do not edit this file manually!\n" +
		"# It will be overwritten by the ALTER SYSTEM command.\n" +
		"synchronized_standby_slots = 'pg_ha_slot_1'\n" +
		"primary_conninfo = 'host=''pg-0.h'' port=5432 user=repmgr dbname=repmgr'\n" +
		"primary_slot_name = 'repmgr_slot_1001'\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveRecoveryConfig(conf)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v, want both recovery GUCs", removed)
	}
	got, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	// Both gone as WHOLE LINES: an empty primary_conninfo is a valid setting meaning "do not
	// stream", so blanking rather than deleting would leave a standby that never connects.
	if strings.Contains(string(got), "primary_conninfo") || strings.Contains(string(got), "primary_slot_name") {
		t.Errorf("a recovery setting survived:\n%s", got)
	}
	// The agent's own ALTER SYSTEM and the header must not be collateral.
	for _, want := range []string{"synchronized_standby_slots = 'pg_ha_slot_1'", "Do not edit this file manually"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("lost %q:\n%s", want, got)
		}
	}
	if perm := mustStat(t, conf).Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %v, want 0600", perm)
	}
	// Idempotent: a second call changes nothing.
	removed2, err := RemoveRecoveryConfig(conf)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed2) != 0 {
		t.Errorf("second call removed %v, want nothing", removed2)
	}
	after, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(got) {
		t.Error("second call rewrote the file")
	}
	// A commented-out setting is not active and must be left alone.
	if err := os.WriteFile(conf, []byte("#primary_conninfo = 'host=x'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveRecoveryConfig(conf); err != nil || len(removed) != 0 {
		t.Errorf("a commented-out setting must be untouched, got (%v, %v)", removed, err)
	}
	// A missing file is not an error: a fresh install has no auto.conf.
	if removed, err := RemoveRecoveryConfig(filepath.Join(dir, "absent.conf")); err != nil || removed != nil {
		t.Errorf("a missing file must be (nil, nil), got (%v, %v)", removed, err)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// PreloadsLibrary is the read-only half of the #293 migration check, and callers scan
// SEVERAL optional paths with it -- postgresql.auto.conf and each conf.d fragment, any
// of which legitimately may not exist. A missing file must therefore be false-with-no-
// error: turning it into an error would fail the boot of every ordinary pod.
func TestPreloadsLibraryOnAMissingFileIsFalseNotAnError(t *testing.T) {
	got, err := PreloadsLibrary(filepath.Join(t.TempDir(), "absent.conf"), "repmgr")
	if err != nil {
		t.Fatalf("a missing optional file must not be an error: %v", err)
	}
	if got {
		t.Error("a missing file cannot request a library")
	}
}

func TestPreloadsLibraryReadsActiveAssignmentsOnly(t *testing.T) {
	for _, c := range []struct {
		name, body string
		lib        string
		want       bool
	}{
		{"sole library", "shared_preload_libraries = 'repmgr'\n", "repmgr", true},
		{"one of several", "shared_preload_libraries = 'pgaudit,repmgr,pg_stat_statements'\n", "repmgr", true},
		{"absent from the list", "shared_preload_libraries = 'pgaudit'\n", "repmgr", false},
		// A commented-out line is not in effect, and treating it as one would refuse a
		// boot over a library nothing asks for.
		{"commented out", "#shared_preload_libraries = 'repmgr'\n", "repmgr", false},
		{"no assignment at all", "wal_log_hints = on\n", "repmgr", false},
		// Substring safety: a longer name that merely contains the short one is a
		// different library.
		{"not a substring match", "shared_preload_libraries = 'repmgrx'\n", "repmgr", false},
	} {
		path := filepath.Join(t.TempDir(), "postgresql.conf")
		if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := PreloadsLibrary(path, c.lib)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: PreloadsLibrary(%q) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

// An unreadable file IS an error: the caller is deciding whether a library the
// postmaster needs is genuinely absent, and "I could not look" must not be reported as
// "it does not ask for it".
func TestPreloadsLibraryReportsAReadFailure(t *testing.T) {
	dir := t.TempDir()
	// A directory where a file is expected reproduces the class (EISDIR) without
	// depending on the test user's privileges.
	if err := os.Mkdir(filepath.Join(dir, "postgresql.conf"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PreloadsLibrary(filepath.Join(dir, "postgresql.conf"), "repmgr"); err == nil {
		t.Fatal("an unreadable file must not be reported as `does not preload`")
	}
}
