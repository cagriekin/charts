package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProc builds a /proc lookalike: one numeric directory per (pid, comm) pair, plus a
// non-numeric entry to prove the scan skips those.
func fakeProc(t *testing.T, comms map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, comm := range comms {
		d := filepath.Join(root, strconv.Itoa(pid))
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPostgresProcessExists(t *testing.T) {
	// The #346 shape: agent is PID 1, one of its threads had the colliding TID -- but
	// threads do not appear in readdir(/proc), so the fixture holds only real processes.
	agentOnly := fakeProc(t, map[int]string{1: "pg-ha-agent"})
	if got, err := postgresProcessExists(agentOnly); err != nil || got {
		t.Errorf("agent-only namespace: got (%v, %v), want (false, nil)", got, err)
	}

	withPG := fakeProc(t, map[int]string{1: "pg-ha-agent", 42: "postgres"})
	if got, err := postgresProcessExists(withPG); err != nil || !got {
		t.Errorf("namespace with a postgres: got (%v, %v), want (true, nil)", got, err)
	}

	// Exact comm match: a shared PID namespace's postgres_exporter (comm truncates to 15
	// bytes) must not read as a live postmaster and block the stale-pid removal.
	exporter := fakeProc(t, map[int]string{1: "pg-ha-agent", 7: "postgres_export"})
	if got, err := postgresProcessExists(exporter); err != nil || got {
		t.Errorf("exporter comm: got (%v, %v), want (false, nil)", got, err)
	}

	if _, err := postgresProcessExists(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("unreadable procRoot must be an error, not \"nothing there\"")
	}
}

// A postgres ZOMBIE must not veto the removal: this PID-1 agent has no reaper, so a
// reparented zombie (e.g. an uncleanly-died pg_ctl bootstrap postmaster) would otherwise
// disarm the stale-pid removal for the rest of the incarnation (#346 review).
func TestPostgresProcessExistsSkipsZombies(t *testing.T) {
	root := fakeProc(t, map[int]string{1: "pg-ha-agent", 42: "postgres"})
	if err := os.WriteFile(filepath.Join(root, "42", "status"), []byte("Name:\tpostgres\nState:\tZ (zombie)\nTgid:\t42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := postgresProcessExists(root); err != nil || got {
		t.Errorf("zombie postgres: got (%v, %v), want (false, nil)", got, err)
	}
	// The same entry in a live state still blocks -- that is the reparented-backend guard.
	if err := os.WriteFile(filepath.Join(root, "42", "status"), []byte("Name:\tpostgres\nState:\tS (sleeping)\nTgid:\t42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := postgresProcessExists(root); err != nil || !got {
		t.Errorf("live postgres with status: got (%v, %v), want (true, nil)", got, err)
	}
}

// A stat failure on postmaster.pid is "could not look", the same state as a failed /proc
// scan, and takes the same policy: leave the file for postgres's own stale-lock check and
// do NOT fail Start -- a transient EIO/ESTALE on a recovering PV must not pin StartLocal.
func TestClearStalePostmasterPidStatErrorDoesNotBlockStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	agentOnly := fakeProc(t, map[int]string{1: "pg-ha-agent"})
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := clearStalePostmasterPid(dir, agentOnly); err != nil {
		t.Fatalf("a stat failure must not block the start: %v", err)
	}
}

// A zombie leader (Tgid == pid, State Z) owns nothing and must not block a wipe: only a
// container restart reaps it here (no PID-1 reaper), so "alive" would mean "refuse until
// restart" (#346 review).
func TestProcessAliveTreatsZombieAsDead(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err == nil && strings.Contains(string(b), "State:\tZ") {
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("child %d did not become a zombie in time", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Errorf("zombie pid %d reported alive; a zombie owns no data directory", pid)
	}
}

func TestClearStalePostmasterPid(t *testing.T) {
	agentOnly := fakeProc(t, map[int]string{1: "pg-ha-agent"})
	withPG := fakeProc(t, map[int]string{1: "pg-ha-agent", 42: "postgres"})

	// Absent file: no-op.
	if err := clearStalePostmasterPid(t.TempDir(), agentOnly); err != nil {
		t.Fatalf("absent pid file: %v", err)
	}

	// Stale file, no postgres anywhere: removed. The recorded PID deliberately names the
	// agent "process" in the fixture -- the very collision postgres's kill(pid, 0) check
	// cannot see through (#346).
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "postmaster.pid")
	if err := os.WriteFile(pidFile, []byte("1\n"+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearStalePostmasterPid(dir, agentOnly); err != nil {
		t.Fatalf("stale pid file: %v", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("a provably stale postmaster.pid must be removed")
	}

	// A postgres process exists: the file is kept for postgres's own interlock to arbitrate.
	if err := os.WriteFile(pidFile, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearStalePostmasterPid(dir, withPG); err != nil {
		t.Fatalf("live namespace: %v", err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Error("postmaster.pid must be kept while any postgres process exists")
	}

	// Scan failure: file kept, start not blocked (postgres applies its own check).
	if err := clearStalePostmasterPid(dir, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("scan failure must not block the start: %v", err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Error("postmaster.pid must be kept when staleness cannot be proven")
	}
}

// End to end through Start: the fresh-start path clears the stale file before exec, so the
// postmaster does not fatal on "lock file \"postmaster.pid\" already exists" (#346).
func TestChildPostmasterStartClearsStalePid(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "postmaster.pid")
	if err := os.WriteFile(pidFile, []byte("24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewChildPostmaster(writeFakePG(t, dir, "exec sleep 30"), dir)
	p.procRoot = fakeProc(t, map[int]string{1: "pg-ha-agent"})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = p.Stop(context.Background(), Immediate) }()
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("Start must remove a provably stale postmaster.pid before exec")
	}
}

func TestChildPostmasterStartKeepsPidWhilePostgresRuns(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "postmaster.pid")
	if err := os.WriteFile(pidFile, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewChildPostmaster(writeFakePG(t, dir, "exec sleep 30"), dir)
	p.procRoot = fakeProc(t, map[int]string{1: "pg-ha-agent", 42: "postgres"})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = p.Stop(context.Background(), Immediate) }()
	if _, err := os.Stat(pidFile); err != nil {
		t.Error("Start must not remove postmaster.pid while a postgres process exists")
	}
}

// processAlive must not read a bare thread TID as a live process -- that is the exact
// confusion postgres's own stale-lock check suffers (#346), and here it wrongly blocks
// WipeDataDir/ClearDebrisDataDir with "PID N is still running".
func TestProcessAliveDistinguishesThreadFromProcess(t *testing.T) {
	self := os.Getpid()
	if !processAlive(self) {
		t.Fatalf("own pid %d must be alive", self)
	}
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Skipf("no /proc/self/task: %v", err)
	}
	for _, e := range tasks {
		tid, aerr := strconv.Atoi(e.Name())
		if aerr != nil || tid == self {
			continue
		}
		if processAlive(tid) {
			t.Errorf("bare thread TID %d reported alive; kill(tid, 0) succeeded but Tgid != tid must rule it out", tid)
		}
		return
	}
	t.Skip("runtime exposed no non-leader thread to test against")
}

// Guard against comm parsing surprises: a trailing newline is what the kernel writes.
func TestPostgresProcessExistsTrimsComm(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "9")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	// No trailing newline at all must also match.
	if err := os.WriteFile(filepath.Join(d, "comm"), []byte("postgres"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := postgresProcessExists(root)
	if err != nil || !got {
		t.Errorf("comm without newline: got (%v, %v), want (true, nil)", got, err)
	}
}
