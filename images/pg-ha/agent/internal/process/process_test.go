package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakePostmaster struct {
	started   bool
	reloaded  bool
	stopMode  StopMode
	stopCalls int
}

func (f *fakePostmaster) Start(context.Context) error  { f.started = true; return nil }
func (f *fakePostmaster) Reload(context.Context) error { f.reloaded = true; return nil }
func (f *fakePostmaster) Running() bool                { return f.started }
func (f *fakePostmaster) Stop(_ context.Context, m StopMode) error {
	f.stopMode = m
	f.stopCalls++
	return nil
}

func TestDemoteUsesImmediateOnFence(t *testing.T) {
	f := &fakePostmaster{}
	s := NewSupervisor(f)
	if err := s.Demote(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if f.stopMode != Immediate {
		t.Errorf("fence demote = %v, want Immediate", f.stopMode)
	}
}

func TestDemoteUsesFastWhenGraceful(t *testing.T) {
	f := &fakePostmaster{}
	s := NewSupervisor(f)
	if err := s.Demote(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if f.stopMode != Fast {
		t.Errorf("graceful demote = %v, want Fast", f.stopMode)
	}
}

// writeFakePG writes an executable stub at dir/fakepg that runs the given shell body.
func writeFakePG(t *testing.T, dir, body string) string {
	t.Helper()
	bin := filepath.Join(dir, "fakepg")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A reconcile tick can call Start while postgres is mid-startup (not yet accepting
// connections, so observe() sees it as not running). Start must be an idempotent
// no-op then, not error with "already started".
func TestChildPostmasterStartIdempotentWhileRunning(t *testing.T) {
	dir := t.TempDir()
	p := NewChildPostmaster(writeFakePG(t, dir, "exec sleep 30"), dir)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	pid := p.cmd.Process.Pid
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("idempotent start while running: %v", err)
	}
	if p.cmd.Process.Pid != pid {
		t.Errorf("idempotent Start replaced the running process (pid %d -> %d)", pid, p.cmd.Process.Pid)
	}
	_ = p.Stop(context.Background(), Immediate)
}

// After postgres exits on its own (crash/OOM), the stale handle must not wedge
// Start on "already started" forever -- the next reconcile tick must restart it.
func TestChildPostmasterRestartsAfterSelfExit(t *testing.T) {
	dir := t.TempDir()
	p := NewChildPostmaster(writeFakePG(t, dir, "exit 0"), dir)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	pid1 := p.cmd.Process.Pid
	// wait until the child's exit is queued on p.exited (len peek does not consume)
	for i := 0; i < 200 && len(p.exited) == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if len(p.exited) == 0 {
		t.Fatal("child did not exit in time")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("restart after self-exit: %v", err)
	}
	if p.cmd.Process.Pid == pid1 {
		t.Errorf("restart after self-exit did not fork a fresh process (pid still %d)", pid1)
	}
	_ = p.Stop(context.Background(), Fast)
}

func TestRecoverySignalSetAndClear(t *testing.T) {
	dir := t.TempDir()
	sig := filepath.Join(dir, "standby.signal")

	// ClearRecoverySignal is a no-op when the file is absent.
	if err := ClearRecoverySignal(dir); err != nil {
		t.Fatalf("clear (absent): %v", err)
	}

	if err := SetRecoverySignal(dir); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := os.Stat(sig); err != nil {
		t.Fatalf("standby.signal not created: %v", err)
	}
	// idempotent
	if err := SetRecoverySignal(dir); err != nil {
		t.Fatalf("set (again): %v", err)
	}

	if err := ClearRecoverySignal(dir); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(sig); !os.IsNotExist(err) {
		t.Fatalf("standby.signal should be gone, stat err = %v", err)
	}
}

func TestHasData(t *testing.T) {
	dir := t.TempDir()
	if HasData(dir) {
		t.Error("empty dir should not have data")
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasData(dir) {
		t.Error("dir with PG_VERSION should have data")
	}
}

// Running() is PROCESS liveness, deliberately distinct from SQL readiness: a
// postmaster replaying WAL toward consistency is Running()=true but still rejects
// connections. The reconcile loop keys on it so a starting standby is not
// misclassified as stopped and needlessly restarted or re-cloned (#181).
func TestChildPostmasterRunningTracksTheProcessNotReadiness(t *testing.T) {
	dir := t.TempDir()
	p := NewChildPostmaster(writeFakePG(t, dir, "exec sleep 30"), dir)
	if p.Running() {
		t.Error("a postmaster that was never started must not report Running")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Alive but accepting nothing: still Running.
	if !p.Running() {
		t.Error("a started postmaster must report Running even before it accepts connections")
	}
	if err := p.Stop(context.Background(), Immediate); err != nil {
		t.Fatal(err)
	}
	if p.Running() {
		t.Error("a stopped postmaster must not report Running")
	}
}

// A process that exits on its own (crash, OOM kill) must stop reporting Running
// without anyone calling Stop -- otherwise the reconcile loop believes a dead node is
// serving and never restarts it.
func TestChildPostmasterRunningGoesFalseOnSelfExit(t *testing.T) {
	dir := t.TempDir()
	p := NewChildPostmaster(writeFakePG(t, dir, "exit 1"), dir)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Running() {
		t.Fatal("a self-exited postmaster still reports Running: the loop would never restart it")
	}
}

// Exited() is how the main loop learns about an unexpected crash. It must be nil
// before the first start (there is nothing to wait on) and must deliver the exit
// afterwards.
func TestChildPostmasterExitedChannel(t *testing.T) {
	dir := t.TempDir()
	p := NewChildPostmaster(writeFakePG(t, dir, "exit 3"), dir)
	if p.Exited() != nil {
		t.Error("Exited() must be nil before the first Start, or a select would fire on a closed nil-case")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the child's exit was never delivered")
	}
}

// Reload is a SIGHUP, which is only meaningful against a live process. On a stopped
// postmaster it must ERROR rather than silently succeed: `ssl` and pg_hba are
// SIGHUP-context, so a caller that believes a reload happened would report a config
// as applied when nothing read it.
func TestChildPostmasterReloadRequiresARunningProcess(t *testing.T) {
	dir := t.TempDir()
	p := NewChildPostmaster(writeFakePG(t, dir, "exec sleep 30"), dir)
	if err := p.Reload(context.Background()); err == nil {
		t.Error("reloading a postmaster that was never started must not report success")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The fake ignores SIGHUP (the default disposition would kill it, so the script
	// traps nothing); what matters is that signalling a live process succeeds.
	if err := p.Reload(context.Background()); err != nil {
		t.Errorf("reloading a running postmaster: %v", err)
	}
	_ = p.Stop(context.Background(), Immediate)
	if err := p.Reload(context.Background()); err == nil {
		t.Error("reloading after Stop must not report success")
	}
}

// Stop on a postmaster that is not running is a no-op, not an error: the reconcile
// loop calls it on paths where the node may already be down (a crash that raced the
// decision), and an error there would be counted as a failed action and retried.
func TestChildPostmasterStopOnAStoppedProcessIsANoOp(t *testing.T) {
	dir := t.TempDir()
	p := NewChildPostmaster(writeFakePG(t, dir, "exec sleep 30"), dir)
	if err := p.Stop(context.Background(), Fast); err != nil {
		t.Errorf("stopping a never-started postmaster must be a no-op: %v", err)
	}
}

// The Supervisor is a thin pass-through, and each verb must reach its own Postmaster
// method: a mis-wired Start/Reload would look correct in review and leave the agent
// signalling a reload where it meant to boot the server.
func TestSupervisorPassesEachVerbThrough(t *testing.T) {
	f := &fakePostmaster{}
	s := NewSupervisor(f)
	ctx := context.Background()
	if s.Running() {
		t.Error("Running must reflect the wrapped postmaster, which has not started")
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.started {
		t.Error("Start did not reach the postmaster")
	}
	if !s.Running() {
		t.Error("Running did not reflect the started postmaster")
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.reloaded {
		t.Error("Reload did not reach the postmaster")
	}
	if err := s.Stop(ctx, Fast); err != nil {
		t.Fatal(err)
	}
	if f.stopCalls != 1 || f.stopMode != Fast {
		t.Errorf("Stop reached the postmaster %d times with mode %v", f.stopCalls, f.stopMode)
	}
}
