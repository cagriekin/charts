package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
//   - no LIVE postmaster owns the data. A postmaster.pid whose recorded PID is still
//     running is a hard refusal. A pid file left by a crashed postmaster is STALE and is
//     removed -- that is precisely the state a replica worth reinitializing is in (only a
//     clean shutdown removes the file), so refusing on its mere presence would make this
//     fail for the case it exists to fix.
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
	if err := checkNoLivePostmaster(clean); err != nil {
		return err
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

// checkNoLivePostmaster refuses when $PGDATA/postmaster.pid names a process that is still
// alive, and tolerates (does not remove -- the wipe that follows deletes it with everything
// else) a pid file whose process is gone.
//
// The distinction matters because a crashed or OOM-killed postmaster leaves its pid file
// behind: only a clean shutdown removes it. Treating that as "something is running here"
// blocks the rebuild of exactly the broken replica this is for. An unreadable or malformed
// pid file is treated as LIVE -- unable to prove it is stale is not permission to proceed.
func checkNoLivePostmaster(dir string) error {
	pidFile := filepath.Join(dir, "postmaster.pid")
	b, err := os.ReadFile(pidFile) //nolint:gosec // path derived from agent config
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refusing to wipe %q: postmaster.pid exists but could not be read (%v), so it cannot be shown to be stale", dir, err)
	}
	// First line is the postmaster's PID.
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0])
	pid, perr := strconv.Atoi(first)
	if perr != nil || pid <= 0 {
		return fmt.Errorf("refusing to wipe %q: postmaster.pid does not contain a usable PID (%q), so it cannot be shown to be stale", dir, first)
	}
	if processAlive(pid) {
		return fmt.Errorf("refusing to wipe %q: postmaster.pid names PID %d, which is still running", dir, pid)
	}
	return nil
}

// processAlive reports whether pid exists in this PID namespace. Signal 0 performs the
// permission and existence checks without delivering anything; an EPERM means the process
// exists but belongs to another user, which still counts as alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// ControlFileMissing reports whether PGDATA has no global/pg_control at all (#288).
//
// pg_basebackup writes pg_control LAST, precisely so an interrupted copy is detectable, so its
// absence is positive evidence that a base backup was cut short. Distinguished from
// "pg_controldata failed" on purpose: that can mean the tool could not run, or that the data
// directory belongs to a different PostgreSQL major, neither of which justifies destroying it.
func ControlFileMissing(pgdata string) bool {
	_, err := os.Stat(filepath.Join(pgdata, "global", "pg_control"))
	return os.IsNotExist(err)
}

// ClearDebrisDataDir empties a data directory that is NOT an initialized cluster --
// entries present but no PG_VERSION -- and reports what it removed. It is the
// complement of WipeDataDir, which refuses exactly this shape.
//
// Why it exists (#298 review, observed live): pg_basebackup demands a byte-empty
// target, while the reconcile loop's "empty data" is HasData, i.e. PG_VERSION. Any
// stray entry in a database-less PGDATA -- a core dump the kernel wrote into a dying
// postmaster's cwd, a clone interrupted before PG_VERSION was written (that shape has
// no complete-marker for discardTornClone to act on), lost+found -- parks the node in
// a permanent loop: every tick decides BootstrapClone, every pg_basebackup refuses
// `directory exists but is not empty`. Nothing here is a database (that is what the
// absent PG_VERSION means), so clearing the entries loses only debris; the removed
// names are returned so the caller can log what was thrown away.
//
// Guards mirror WipeDataDir's, inverted where the shape differs: absolute and >=2
// segments deep; must currently exist and be a directory; PG_VERSION must be ABSENT
// (an initialized cluster is WipeDataDir's territory and is refused here); and no
// LIVE postmaster may own the directory -- unlikely without PG_VERSION, but "cannot
// prove it is stale" stays a refusal, not permission.
//
// A missing directory is a NO-OP, deliberately unlike WipeDataDir: on a fresh native
// install PGDATA does not exist until pg_basebackup -D creates it, and that is the very
// clone this runs in front of -- erroring here would block every first clone. (An absent
// volume mount fails a moment later in pg_basebackup, with its own message.)
func ClearDebrisDataDir(dir string) ([]string, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("refusing to clear %q: not an absolute path", dir)
	}
	clean := filepath.Clean(dir)
	if depth := len(strings.Split(strings.Trim(clean, "/"), "/")); clean == "/" || depth < 2 {
		return nil, fmt.Errorf("refusing to clear %q: too close to the filesystem root to be a data directory", clean)
	}
	fi, err := os.Stat(clean)
	if os.IsNotExist(err) {
		return nil, nil // nothing there yet; pg_basebackup -D creates it
	}
	if err != nil {
		return nil, fmt.Errorf("refusing to clear %q: %w", clean, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("refusing to clear %q: not a directory", clean)
	}
	if _, serr := os.Stat(filepath.Join(clean, "PG_VERSION")); serr == nil {
		return nil, fmt.Errorf("refusing to clear %q: PG_VERSION present, this is an initialized data directory (WipeDataDir is the destructive path for those)", clean)
	}
	if err := checkNoLivePostmaster(clean); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", clean, err)
	}
	removed := make([]string, 0, len(entries))
	for _, e := range entries {
		p := filepath.Join(clean, e.Name())
		if rerr := os.RemoveAll(p); rerr != nil {
			return removed, fmt.Errorf("remove %q: %w", p, rerr)
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}
