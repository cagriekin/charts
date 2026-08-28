// Package atomicfile writes a file so that a crash mid-write cannot leave a reader seeing
// half of it. Every torn-file argument in this codebase rests on this one function
// (#298 review): there were two implementations -- pgconf's ([]byte) and mechanism's
// (string) -- and they had drifted. Only one fsynced the parent directory as a hard
// requirement while the other made it best-effort, so "the write is atomic AND durable"
// was true of postgresql.conf and only half-true of primary_conninfo, in a codebase where
// a truncated postgresql.conf is a postmaster that will not start at all.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes data to path via a temp file in the same directory, fsync, and rename.
//
// The order is load-bearing, and each step answers a different failure:
//
//   - same-directory temp: rename is only atomic within a filesystem.
//   - fsync BEFORE the rename: rename gives NAME atomicity, not content durability. Without
//     it a crash shortly after the rename can leave a correctly-named ZERO-LENGTH file,
//     which for postgresql.conf is worse than the torn write it replaced.
//   - fsync the DIRECTORY after: otherwise the rename itself can be lost, leaving the old
//     content (or nothing) behind.
//
// The directory fsync is best-effort: some filesystems refuse to sync an O_RDONLY directory,
// and failing the whole write there would trade a durability gap that the next reconcile tick
// rewrites anyway for an error that stops the tick. Everything before it is fatal.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// Chmod rather than relying on CreateTemp's 0600: callers pass the mode the reader
	// requires, and PostgreSQL refuses to start when a file in PGDATA is group/world readable.
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// WriteString is Write for the callers that already hold a string.
func WriteString(path, content string, perm os.FileMode) error {
	return Write(path, []byte(content), perm)
}
