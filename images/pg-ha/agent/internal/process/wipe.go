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

// wipeProcRoot is /proc in production. A variable only as a test seam: the machine running
// the unit tests may itself host a live postgres, which would otherwise flip every
// stale-pid-file wipe test to a refusal.
var wipeProcRoot = "/proc"

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
	// A dead recorded PID does not prove nothing owns the data (#346 review): a SIGKILLed
	// postmaster is denied the chance to reap its backends, so they can survive it --
	// still attached to the shared memory and data files this wipe is about to delete --
	// while the pid file names only their dead parent. The namespace scan the stale-pid
	// removal uses (stalepid.go) sees them; apply it here too. Unlike that removal, a
	// failed scan is a REFUSAL: this path deletes a database, so "could not look" is not
	// permission (clearStalePostmasterPid may fail open because its kept file is
	// re-arbitrated by postgres; nothing re-arbitrates a wipe).
	alive, aerr := postgresProcessExists(wipeProcRoot)
	if aerr != nil {
		return fmt.Errorf("refusing to wipe %q: postmaster.pid exists and the postgres process scan failed (%v), so it cannot be shown to be stale", dir, aerr)
	}
	if alive {
		return fmt.Errorf("refusing to wipe %q: postmaster.pid's recorded PID %d is gone, but a live postgres process still exists in this PID namespace (a surviving backend of a killed postmaster?)", dir, pid)
	}
	return nil
}

// processAlive reports whether pid names a live PROCESS in this PID namespace. Signal 0
// performs the permission and existence checks without delivering anything; an EPERM
// establishes existence only -- the /proc proof below still runs, because a bare thread
// TID of ANOTHER user's process also yields EPERM (#346 review) and /proc/<pid>/status
// is world-readable, so the proof is available even when the signal is not permitted.
//
// Existence alone is not enough (#346): on Linux kill(tid, 0) also succeeds for a bare
// THREAD id, and after a container restart the agent (PID 1) owns low TIDs that collide
// with the PID a previous incarnation's postmaster.pid recorded -- the wipe then refuses
// forever on "PID N is still running" when N is one of the agent's own goroutine threads.
// A real process is its thread-group leader (Tgid == pid in /proc/<pid>/status); a bare
// thread is not something that can own a data directory -- and neither is a zombie
// (State Z): it is dead-in-waiting with no files, locks or shared memory, and this
// reaper-less PID-1 agent keeps reparented zombies for the whole incarnation (see Stop
// in postmaster.go), so counting one alive would block the wipe until a container
// restart. An unreadable or unparsable status still counts as alive: unable to prove it
// is a mere thread (or a zombie) is not permission to proceed.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false // ESRCH: no process or thread has this id at all
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)) //nolint:gosec // fixed path, numeric pid
	if err != nil {
		// The id can vanish between kill(0) and this read -- a retiring runtime thread
		// is exactly the #346 shape -- so re-check before blaming the pid: gone is
		// gone; still signalable with an unreadable status stays alive (fail closed).
		kerr := syscall.Kill(pid, 0)
		return kerr == nil || errors.Is(kerr, syscall.EPERM)
	}
	tgid := -1
	zombie := false
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Tgid:"); ok {
			n, aerr := strconv.Atoi(strings.TrimSpace(v))
			if aerr != nil {
				return true // unparsable: cannot prove it is a mere thread
			}
			tgid = n
		} else if v, ok := strings.CutPrefix(line, "State:"); ok {
			zombie = strings.HasPrefix(strings.TrimSpace(v), "Z")
		}
	}
	if tgid == -1 {
		return true // no Tgid line: cannot prove it is a mere thread
	}
	return tgid == pid && !zombie
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
	// FAIL CLOSED on any stat error that is not "absent". Only os.IsNotExist proves
	// PG_VERSION is genuinely missing; EIO on a degraded volume, ESTALE on an NFS-backed
	// PV or ELOOP all mean "cannot tell", and reading those as absence let this function
	// os.RemoveAll an initialized PGDATA -- the exact directory the guard exists to
	// protect, on the BootstrapClone path that runs it in front of pg_basebackup. The
	// sibling WipeDataDir already refuses on any error; match it.
	if _, serr := os.Stat(filepath.Join(clean, "PG_VERSION")); serr == nil {
		return nil, fmt.Errorf("refusing to clear %q: PG_VERSION present, this is an initialized data directory (WipeDataDir is the destructive path for those)", clean)
	} else if !os.IsNotExist(serr) {
		return nil, fmt.Errorf("refusing to clear %q: cannot determine whether PG_VERSION is present: %w", clean, serr)
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
