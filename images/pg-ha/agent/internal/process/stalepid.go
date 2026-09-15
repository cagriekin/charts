package process

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// clearStalePostmasterPid removes $PGDATA/postmaster.pid before a fresh start when it is
// PROVABLY stale: no process named `postgres` exists in this PID namespace, so the file
// cannot name a live postmaster no matter what PID it records.
//
// Why postgres cannot be left to make this call itself (#346): its stale-lock check is
// kill(pid, 0), and on Linux that succeeds for a bare THREAD id too. After a hard
// power-off the container restarts with the agent as PID 1, PIDs restart from 1, and one
// of the agent's own Go runtime threads can land on the TID the previous incarnation's
// postmaster recorded in the file. postgres then concludes a postmaster is alive
// ("lock file \"postmaster.pid\" already exists ... Is another postmaster (PID N)
// running") and refuses to start -- on every StartLocal tick, forever, on the lease
// holder. The agent, which spawned no postmaster in this incarnation, is the one party
// that can prove the file stale.
//
// The proof is a scan of /proc for any live process whose comm is exactly `postgres`
// (pgrep -x postgres): readdir(/proc) enumerates thread-group leaders only, so an agent
// thread's TID does not count, while a postmaster -- or any live backend a SIGKILLed
// postmaster left behind, reparented to this agent -- does, and blocks the removal.
// Unreaped postgres ZOMBIES do not block (see postgresProcessExists): they own nothing,
// and this reaper-less PID-1 agent keeps them around for the whole incarnation.
// Callers reach here only from ChildPostmaster.Start's fresh path, where any child this
// agent spawned has already been reaped.
//
// Fail-safe in both directions: if the file is kept (a postgres process exists, or the
// scan errored), postgres's own interlock still refuses a genuinely live conflict; the
// removal happens only on evidence that would also make `rm postmaster.pid` safe by hand.
func clearStalePostmasterPid(dataDir, procRoot string) error {
	if procRoot == "" {
		// Default rather than error (#346 review): a zero-value ChildPostmaster (only
		// NewChildPostmaster constructs one today, but the zero value is otherwise fully
		// usable) would otherwise fail every scan and silently degrade this fix to a
		// warn-and-skip.
		procRoot = "/proc"
	}
	pidFile := filepath.Join(dataDir, "postmaster.pid")
	if _, err := os.Stat(pidFile); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		// Same epistemic state as a failed scan below -- could not look -- so same
		// policy: do not block the start. A transient EIO/ESTALE on a PV recovering
		// from the very power-off this handles (#346) must not pin StartLocal on a
		// stat that postgres's own lock-file open may well outlive; postgres applies
		// its stale-lock check and the next tick retries.
		slog.Warn("could not stat postmaster.pid; leaving any file for postgres's own stale-lock check", "pidFile", pidFile, "err", err)
		return nil
	}
	alive, err := postgresProcessExists(procRoot)
	if err != nil {
		// Cannot prove staleness -> do not remove, but do not block the start either:
		// postgres applies its own (weaker) check and the next tick retries.
		slog.Warn("postmaster.pid exists but the process scan failed; leaving the file for postgres's own stale-lock check", "pidFile", pidFile, "err", err)
		return nil
	}
	if alive {
		// A postgres process is running in this namespace. It is not our child (the
		// caller's fresh-start path proves that), so the file may be its lock -- keep it
		// and let postgres's interlock arbitrate.
		return nil
	}
	// Best-effort read of the recorded PID before the remove: this deletes a database
	// lock file, and post-incident forensics deserve to know which PID the file blamed.
	recorded := ""
	if b, rerr := os.ReadFile(pidFile); rerr == nil { //nolint:gosec // path derived from agent config
		recorded = strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0])
	}
	if err := os.Remove(pidFile); err != nil {
		if os.IsNotExist(err) {
			return nil // vanished between the stat and here; nothing was removed, log nothing
		}
		return fmt.Errorf("remove stale %s: %w", pidFile, err)
	}
	slog.Warn("removed a stale postmaster.pid left by a previous container incarnation: no postgres process exists in this PID namespace, and this agent has spawned none (#346)", "pidFile", pidFile, "recordedPid", recorded)
	return nil
}

// ClearStalePostmasterPid is clearStalePostmasterPid against the real /proc, for callers
// that are about to run postgres on dataDir WITHOUT going through ChildPostmaster.Start
// (#346 review). Today that is the rejoin path: pg_rewind finishes the target's crash
// recovery with `postgres --single` (PG13+, no --no-ensure-shutdown anywhere), and
// single-user mode applies the same kill(pid, 0) stale-lock check the TID collision
// defeats -- so without this, every rewind on a hard-powered-off ex-primary fails with
// "lock file already exists" (neither divergence nor a connection error), rejoinOnto
// counts three such failures, and the node pays a full ReclonePreserving for a file the
// agent can prove stale.
func ClearStalePostmasterPid(dataDir string) error {
	return clearStalePostmasterPid(dataDir, "/proc")
}

// postgresProcessExists reports whether any LIVE process in this PID namespace has comm
// exactly `postgres` (the postmaster and every backend are so named; comm truncates at
// 15 bytes, well past 8, and exact match keeps postgres_exporter and friends out).
// procRoot is /proc in production, injectable for tests. A process vanishing mid-scan is
// skipped; failure to enumerate procRoot at all is an error, because "could not look" is
// not "nothing there".
//
// Zombies are skipped (#346 review): a zombie is dead-in-waiting -- no lock file, no
// shared memory, no open files -- and because this agent is PID 1 with no reaper (see
// Stop in postmaster.go), a reparented postgres zombie can linger for the rest of the
// incarnation. Counting it alive would disarm this removal forever after any SIGKILL
// fence, and would let a pg_ctl-daemonized bootstrap postmaster that died uncleanly veto
// the removal of its OWN stale file -- the very "lock file already exists" loop this
// exists to break. Live backends (any non-Z state) still block, which is the
// reparented-backend guard the scan is for; an unreadable state counts as live, because
// keeping the file is the safe direction.
func postgresProcessExists(procRoot string) (bool, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", procRoot, err)
	}
	for _, e := range entries {
		if pid, aerr := strconv.Atoi(e.Name()); aerr != nil || pid <= 0 {
			continue
		}
		comm, rerr := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm")) //nolint:gosec // procRoot is /proc (or a test fixture); entry names are numeric
		if rerr != nil {
			continue // exited between readdir and read
		}
		if strings.TrimSpace(string(comm)) != "postgres" {
			continue
		}
		if isZombie(procRoot, e.Name()) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// isZombie reports whether /proc/<name>/status shows State Z. Unknown -- unreadable or
// unparsable status, as in the minimal test fixtures -- is NOT a zombie: the caller then
// counts the process as live and keeps the pid file, the fail-safe direction.
func isZombie(procRoot, name string) bool {
	b, err := os.ReadFile(filepath.Join(procRoot, name, "status")) //nolint:gosec // procRoot is /proc (or a test fixture); name is numeric
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "State:"); ok {
			return strings.HasPrefix(strings.TrimSpace(v), "Z")
		}
	}
	return false
}
