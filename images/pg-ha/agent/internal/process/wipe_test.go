package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initDataDir builds something that looks like a real PGDATA.
func initDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pgdata")
	if err := os.MkdirAll(filepath.Join(dir, "base", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"PG_VERSION", "postgresql.conf", filepath.Join("base", "1", "2657")} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestWipeDataDirEmptiesButKeepsTheDirectory(t *testing.T) {
	dir := initDataDir(t)
	if err := WipeDataDir(dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The directory itself must survive: it is a volume mount (or inside one), and
	// removing it would break the mount for the clone that follows.
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("the data directory itself must remain: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("directory should be empty, still has %d entries", len(entries))
	}
	// And with PG_VERSION gone, the reconcile loop sees "empty data" and re-clones.
	if HasData(dir) {
		t.Error("HasData must be false after a wipe, or the loop will not re-clone")
	}
}

// The interlock that stops this wiping a database out from under a running postmaster.
func TestWipeDataDirRefusesWithPostmasterPid(t *testing.T) {
	dir := initDataDir(t)
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WipeDataDir(dir)
	if err == nil {
		t.Fatal("a present postmaster.pid must block the wipe")
	}
	if !strings.Contains(err.Error(), "postmaster.pid") {
		t.Errorf("error should name the interlock: %v", err)
	}
	if !HasData(dir) {
		t.Error("nothing may have been removed")
	}
}

// It must not empty a directory that merely happens to be named in the config.
func TestWipeDataDirRefusesNonDataDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-pgdata")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(dir, "someone-elses-file")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WipeDataDir(dir)
	if err == nil {
		t.Fatal("a directory without PG_VERSION must not be wiped")
	}
	if !strings.Contains(err.Error(), "PG_VERSION") {
		t.Errorf("error should say why: %v", err)
	}
	if _, serr := os.Stat(precious); serr != nil {
		t.Error("the unrelated file must be untouched")
	}
}

// A misconfigured or empty PGDATA must never resolve to something near the root.
func TestWipeDataDirRefusesShallowPaths(t *testing.T) {
	for _, p := range []string{"/", "/var", "/var/", "//", "/x"} {
		if err := WipeDataDir(p); err == nil {
			t.Errorf("WipeDataDir(%q) must be refused", p)
		} else if !strings.Contains(err.Error(), "filesystem root") {
			t.Errorf("WipeDataDir(%q): error should name the reason: %v", p, err)
		}
	}
}

func TestWipeDataDirRefusesRelativePath(t *testing.T) {
	if err := WipeDataDir("pgdata"); err == nil {
		t.Error("a relative path must be refused")
	}
}

// A path that does not exist is an error, not a quiet success: the caller asked to
// reinitialize a replica, and "nothing to do" would hide a misconfiguration.
func TestWipeDataDirMissingPathIsAnError(t *testing.T) {
	if err := WipeDataDir(filepath.Join(t.TempDir(), "a", "absent")); err == nil {
		t.Error("a missing data directory must be an error")
	}
}

func TestWipeDataDirRefusesFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "sub", "file")
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WipeDataDir(f); err == nil {
		t.Error("a regular file must be refused")
	}
}

// A crashed or OOM-killed postmaster leaves its pid file behind -- only a clean shutdown
// removes it -- so that is exactly the state a replica worth reinitializing is in. Refusing
// on the file's mere presence made the feature fail for its own main use case.
func TestWipeDataDirTolieratesAStalePidFile(t *testing.T) {
	dir := initDataDir(t)
	// A PID that cannot be running: pid 0 is never a user process, so use a very high one
	// that is free. Verify it is genuinely absent before relying on it.
	stale := 4194303
	for processAlive(stale) {
		stale--
	}
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"),
		[]byte(fmt.Sprintf("%d\n/var/lib/postgresql/data/pgdata\n", stale)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WipeDataDir(dir); err != nil {
		t.Fatalf("a stale pid file must not block the rebuild: %v", err)
	}
	if HasData(dir) {
		t.Error("the directory should have been emptied")
	}
}

// A pid file naming a LIVE process is a hard refusal: something owns this data.
func TestWipeDataDirRefusesALivePidFile(t *testing.T) {
	dir := initDataDir(t)
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WipeDataDir(dir)
	if err == nil {
		t.Fatal("a live postmaster.pid must block the wipe")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("error should say the process is alive: %v", err)
	}
	if !HasData(dir) {
		t.Error("nothing may have been removed")
	}
}

// Unable to prove staleness is not permission to proceed.
func TestWipeDataDirRefusesUnreadablePidFile(t *testing.T) {
	for _, body := range []string{"", "not-a-pid\n", "0\n", "-5\n"} {
		dir := initDataDir(t)
		if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WipeDataDir(dir); err == nil {
			t.Errorf("pid file %q must be refused, not assumed stale", body)
		} else if !strings.Contains(err.Error(), "stale") {
			t.Errorf("pid file %q: error should explain: %v", body, err)
		}
		if !HasData(dir) {
			t.Errorf("pid file %q: nothing may have been removed", body)
		}
	}
}

// --- ClearDebrisDataDir (#298 review): the pre-clone debris path ---

// The wedge this exists for: entries present, PG_VERSION absent -- pg_basebackup would
// refuse the directory forever while Decide keeps choosing BootstrapClone.
func TestClearDebrisDataDirRemovesDebrisAndNamesIt(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"core.89", "lost+found"} {
		if err := os.MkdirAll(filepath.Join(dir, f), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := ClearDebrisDataDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("expected 2 removed entries, got %v", removed)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("directory should be empty, still has %d entries", len(entries))
	}
	// The directory itself must survive: it is a volume mount (or inside one).
	if fi, serr := os.Stat(dir); serr != nil || !fi.IsDir() {
		t.Fatalf("the data directory itself must remain: %v", serr)
	}
}

// An initialized cluster is WipeDataDir's territory; this must never touch one.
func TestClearDebrisDataDirRefusesInitializedCluster(t *testing.T) {
	dir := initDataDir(t)
	_, err := ClearDebrisDataDir(dir)
	if err == nil {
		t.Fatal("PG_VERSION present must block the clear")
	}
	if !strings.Contains(err.Error(), "PG_VERSION") {
		t.Errorf("error should name PG_VERSION: %v", err)
	}
	if !HasData(dir) {
		t.Error("nothing may have been removed")
	}
}

// A missing PGDATA is the normal fresh-install shape: pg_basebackup -D creates it, so
// erroring here would block every first clone.
func TestClearDebrisDataDirMissingPathIsANoOp(t *testing.T) {
	removed, err := ClearDebrisDataDir(filepath.Join(t.TempDir(), "not-created-yet"))
	if err != nil {
		t.Fatalf("a missing directory must be a no-op, got: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("nothing to remove from a missing directory, got %v", removed)
	}
}

// An empty directory is equally a no-op (the common re-clone shape after a wipe).
func TestClearDebrisDataDirEmptyDirIsANoOp(t *testing.T) {
	removed, err := ClearDebrisDataDir(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("expected no removals, got %v", removed)
	}
}

// "Cannot prove it is stale" stays a refusal even on this path.
func TestClearDebrisDataDirRefusesALivePidFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearDebrisDataDir(dir); err == nil {
		t.Fatal("a live postmaster.pid must block the clear")
	}
	if _, err := os.Stat(filepath.Join(dir, "postmaster.pid")); err != nil {
		t.Error("nothing may have been removed")
	}
}

// The path-shape guards hold on this entry point too.
func TestClearDebrisDataDirRefusesShallowOrRelativePaths(t *testing.T) {
	for _, p := range []string{"/", "/var", "relative/path"} {
		if _, err := ClearDebrisDataDir(p); err == nil {
			t.Errorf("expected refusal for %q", p)
		}
	}
}
