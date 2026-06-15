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

func (f *fakePostmaster) Start(context.Context) error { f.started = true; return nil }
func (f *fakePostmaster) Reload(context.Context) error { f.reloaded = true; return nil }
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
