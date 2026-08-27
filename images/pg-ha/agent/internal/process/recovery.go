package process

import (
	"fmt"
	"os"
	"path/filepath"
)

// standbySignal is the PG12+ file that forces a server to start in standby
// (read-only recovery) mode instead of opening read-write. The agent uses it to
// bring a primary-state data dir up read-only so its true end-of-WAL is
// observable for the cold-boot election, without risking a second writer.
const standbySignal = "standby.signal"

// SetRecoverySignal creates standby.signal in dataDir (idempotent), so the next
// start replays the local WAL to its true end and stays read-only until promoted.
func SetRecoverySignal(dataDir string) error {
	p := filepath.Join(dataDir, standbySignal)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", p, err)
	}
	return f.Close()
}

// ClearRecoverySignal removes standby.signal in dataDir if present, so the next
// start opens read-write (a primary resuming via crash recovery, same timeline --
// no promotion, no timeline bump). A missing file is not an error.
func ClearRecoverySignal(dataDir string) error {
	p := filepath.Join(dataDir, standbySignal)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	return nil
}
