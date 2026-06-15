package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
