package process

import (
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
