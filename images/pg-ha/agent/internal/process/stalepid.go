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
// The proof is a scan of /proc for any process whose comm is exactly `postgres`
// (pgrep -x postgres): readdir(/proc) enumerates thread-group leaders only, so an agent
// thread's TID does not count, while a postmaster -- or any backend a SIGKILLed
// postmaster left behind, reparented to this agent -- does, and blocks the removal.
// Callers reach here only from ChildPostmaster.Start's fresh path, where any child this
// agent spawned has already been reaped.
//
// Fail-safe in both directions: if the file is kept (a postgres process exists, or the
// scan errored), postgres's own interlock still refuses a genuinely live conflict; the
// removal happens only on evidence that would also make `rm postmaster.pid` safe by hand.
func clearStalePostmasterPid(dataDir, procRoot string) error {
	pidFile := filepath.Join(dataDir, "postmaster.pid")
	if _, err := os.Stat(pidFile); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", pidFile, err)
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
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale %s: %w", pidFile, err)
	}
	slog.Warn("removed a stale postmaster.pid left by a previous container incarnation: no postgres process exists in this PID namespace, and this agent has spawned none (#346)", "pidFile", pidFile)
	return nil
}

// postgresProcessExists reports whether any process in this PID namespace has comm
// exactly `postgres` (the postmaster and every backend are so named; comm truncates at
// 15 bytes, well past 8, and exact match keeps postgres_exporter and friends out).
// procRoot is /proc in production, injectable for tests. A process vanishing mid-scan is
// skipped; failure to enumerate procRoot at all is an error, because "could not look" is
// not "nothing there".
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
		if strings.TrimSpace(string(comm)) == "postgres" {
			return true, nil
		}
	}
	return false, nil
}
