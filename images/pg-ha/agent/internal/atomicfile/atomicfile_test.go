package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteContentPermAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.conf")
	if err := WriteString(path, "first\n", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteString(path, "second\n", 0o640); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "second\n" {
		t.Errorf("content = %q, want the second write", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The mode the CALLER asked for, not CreateTemp's 0600: PostgreSQL refuses to start when a
	// file in PGDATA is group/world readable, so silently keeping the temp file's mode would
	// turn a config write into a startup failure.
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("perm = %04o, want 0640", got)
	}
}

// A failed or completed write must leave no temp file behind. These land in PGDATA, where a
// stray `postgresql.conf.tmp-*` is at best confusing to an operator reading the directory and
// at worst something a glob picks up.
func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteString(filepath.Join(dir, "pg_hba.conf"), "local all all trust\n", 0o600); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file survived the write: %s", e.Name())
		}
	}
	if len(ents) != 1 {
		t.Errorf("expected exactly the target file, got %d entries", len(ents))
	}
}

// An unwritable directory must surface an error rather than reporting success -- the callers
// treat a nil return as "the config on disk is now what I asked for".
func TestWriteFailsWhenTheDirectoryIsUnwritable(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	if err := os.Mkdir(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := WriteString(filepath.Join(sub, "f.conf"), "x\n", 0o600); err == nil {
		t.Error("expected an error writing into a read-only directory")
	}
}
