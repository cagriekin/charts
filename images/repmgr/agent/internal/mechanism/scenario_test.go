package mechanism

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The scenario table for the Mechanism interface (#287/#13). The reconcile loop and
// main.go's act() hold only the interface (see iface_assert.go) -- they cannot tell which
// implementation they are driving, so any contract asserted here must hold for any of them.
//
// It covered two implementations until #294 deleted mechanism.Repmgr; the table shape is
// kept rather than inlined into native_test.go, because it is the only place that states
// which guarantees are the INTERFACE'S rather than one backend's, and it is what a second
// implementation would be dropped into. What belongs here, because the interface's own doc
// comments promise it regardless of backend: data-safety and idempotency --
// ReclonePreserving must never lose data (#175), and GenerateConfig must be safe to call on
// every reconcile tick. Behaviour dictated by one backend's own output (repmgr's #182
// "already following" no-op, its #297 local-vs-upstream record distinction) never did.

// mechCase builds a fresh Mechanism against dataDir for one scenario. failing selects
// whether the underlying Runner should fail the operation under test.
type mechCase struct {
	name  string
	build func(t *testing.T, dataDir string, failing bool, now Clock) Mechanism
}

func mechCases() []mechCase {
	return []mechCase{
		{"native", func(t *testing.T, dataDir string, failing bool, now Clock) Mechanism {
			failOn := ""
			if failing {
				failOn = "--checkpoint=fast" // native's Clone runs pg_basebackup ... --checkpoint=fast
			}
			if err := os.WriteFile(filepath.Join(dataDir, "postgresql.conf"), []byte("# initial\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return &Native{Runner: &fakeRunner{failOn: failOn}, DataDir: dataDir,
				PGBindir: "/usr/lib/postgresql/18/bin", Now: now}
		}},
	}
}

// #175: ReclonePreserving must never rm -rf before a clone succeeds. A failed clone
// must leave the diverged data recoverable at a named, predictable path for both
// mechanisms -- this is the single most safety-critical contract the interface has,
// and it is independently implemented (not shared code) by each mechanism.
func TestSharedReclonePreservingKeepsDataOnCloneFailure(t *testing.T) {
	for _, c := range mechCases() {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dataDir, "PG_VERSION"), []byte("18"), 0o600); err != nil {
				t.Fatal(err)
			}
			fixed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
			m := c.build(t, dataDir, true, func() time.Time { return fixed })

			err := m.ReclonePreserving(context.Background(), Conn{Host: "src", User: "u", DB: "d"})
			if err == nil {
				t.Fatal("expected the clone failure to surface")
			}
			backup := strings.TrimRight(dataDir, "/") + ".diverged.20260615T120000Z"
			if _, statErr := os.Stat(filepath.Join(backup, "PG_VERSION")); statErr != nil {
				t.Errorf("diverged data not preserved at %s: %v", backup, statErr)
			}
		})
	}
}

// #175 (success leg): the backup is dropped only once the clone actually succeeds.
func TestSharedReclonePreservingDropsBackupOnSuccess(t *testing.T) {
	for _, c := range mechCases() {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			fixed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
			m := c.build(t, dataDir, false, func() time.Time { return fixed })

			if err := m.ReclonePreserving(context.Background(), Conn{Host: "src", User: "u", DB: "d"}); err != nil {
				t.Fatal(err)
			}
			backup := strings.TrimRight(dataDir, "/") + ".diverged.20260615T120000Z"
			if _, statErr := os.Stat(backup); !os.IsNotExist(statErr) {
				t.Errorf("backup should be removed on clone success, stat err = %v", statErr)
			}
		})
	}
}

// GenerateConfig runs every reconcile tick's role-reconciliation path (main.go), so it
// must be safe to call repeatedly without error or unbounded accumulation, for both
// mechanisms.
func TestSharedGenerateConfigIsIdempotent(t *testing.T) {
	for _, c := range mechCases() {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			m := c.build(t, dataDir, false, time.Now)
			id := NodeIdentity{NodeName: "pg-0", FQDN: "pg-0.h", DataDir: dataDir,
				PGBindir: "/usr/lib/postgresql/18/bin", ReplUser: "repmgr", ReplDB: "repmgr", ReplPassword: "pw"}
			opts := ConfigOpts{Failover: "manual", UseReplicationSlots: true}
			if err := m.GenerateConfig(context.Background(), id, opts); err != nil {
				t.Fatalf("first call: %v", err)
			}
			if err := m.GenerateConfig(context.Background(), id, opts); err != nil {
				t.Fatalf("second call: %v", err)
			}
		})
	}
}
