package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WipeDataDir empties an initialized PostgreSQL data directory, leaving the directory
// itself in place (it is a volume mount, or a subdirectory of one, and removing it would
// break the mount). It is the destructive half of the control API's reinitialize
// operation: once PGDATA is empty the reconcile loop's ordinary "empty data, not the
// chosen primary -> clone from the lease holder" path rebuilds the replica, so no
// separate clone logic exists to get wrong.
//
// Every guard here exists because this function deletes a database. It refuses unless:
//
//   - dir is absolute and at least two path segments deep, so a misconfigured or empty
//     PGDATA cannot turn into "/" or "/var";
//   - dir currently holds PG_VERSION, i.e. it really is an initialized data directory --
//     this function will not empty an arbitrary directory that merely happens to be
//     named in the config;
//   - no postmaster.pid is present. The caller stops PostgreSQL first, so a pid file here
//     means something is (or recently was) running on this data, and wiping underneath it
//     is never right. Same interlock pgBackRest applies before a restore.
//
// A missing directory is an error rather than a no-op: the caller asked to reinitialize a
// replica, and silently succeeding on a path that does not exist would hide a
// misconfiguration behind an apparently-fine result.
func WipeDataDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("refusing to wipe %q: not an absolute path", dir)
	}
	clean := filepath.Clean(dir)
	if depth := len(strings.Split(strings.Trim(clean, "/"), "/")); clean == "/" || depth < 2 {
		return fmt.Errorf("refusing to wipe %q: too close to the filesystem root to be a data directory", clean)
	}
	fi, err := os.Stat(clean)
	if err != nil {
		return fmt.Errorf("refusing to wipe %q: %w", clean, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to wipe %q: not a directory", clean)
	}
	if _, serr := os.Stat(filepath.Join(clean, "PG_VERSION")); serr != nil {
		return fmt.Errorf("refusing to wipe %q: no PG_VERSION, so this is not an initialized PostgreSQL data directory", clean)
	}
	if _, perr := os.Stat(filepath.Join(clean, "postmaster.pid")); perr == nil {
		return fmt.Errorf("refusing to wipe %q: postmaster.pid is present, so PostgreSQL is (or was) running on this data", clean)
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return fmt.Errorf("read %q: %w", clean, err)
	}
	for _, e := range entries {
		p := filepath.Join(clean, e.Name())
		if rerr := os.RemoveAll(p); rerr != nil {
			// Report the first failure with its path: a partially emptied directory is
			// still safe (the loop re-clones only a directory with no PG_VERSION, and a
			// half-wiped one is retried), but the operator needs to know which entry
			// blocked it.
			return fmt.Errorf("remove %q: %w", p, rerr)
		}
	}
	return nil
}
