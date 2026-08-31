package process

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cagriekin/pg-ha-agent/internal/atomicfile"
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
	// Through atomicfile, like Native.Follow's write of the same file (#298 review). A plain
	// OpenFile+Close fsyncs neither the file nor PGDATA, and this is the ONE path whose whole
	// purpose is "bring primary-state data up read-only without risking a second writer":
	// StartRecovery creates the signal and immediately starts the postmaster, which then makes
	// the CONTROL FILE durable on its own. On ext4 data=ordered the directory entry for
	// standby.signal can still be sitting in the 5-second commit window, so a power loss there
	// comes back with a durable control file saying "in archive recovery" and no signal file --
	// exactly the recovery-state-without-signal shape that boot() and StartLocal must not start
	// read-write. atomicfile's package doc calls out that two drifted implementations of this
	// write were the defect it exists to fix; this was the third.
	if err := atomicfile.WriteString(p, "", 0o600); err != nil {
		return fmt.Errorf("create %s: %w", p, err)
	}
	return nil
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
