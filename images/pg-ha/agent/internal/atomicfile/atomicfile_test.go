package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// The whole point: a reader never sees a partial file. The rename is the only moment
// the target changes, so a reader racing the write observes either the old bytes or the
// new ones -- never a prefix. That is what every torn-file argument in this codebase
// rests on, and postgresql.conf is the file where a prefix means a postmaster that will
// not start at all.
func TestConcurrentReadersNeverSeeAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.conf")
	old := strings.Repeat("a", 64<<10) + "\n"
	newer := strings.Repeat("b", 64<<10) + "\n"
	if err := WriteString(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	bad := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				continue // the target is never absent, but an EINTR-class read is not the point here
			}
			if s := string(b); s != old && s != newer {
				select {
				case bad <- s[:min(len(s), 40)]:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 50; i++ {
		content := old
		if i%2 == 1 {
			content = newer
		}
		if err := WriteString(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case s := <-bad:
		t.Fatalf("a reader observed a file that is neither version (starts %q): the write was not atomic", s)
	default:
	}
}

// The temp file is created in the TARGET's directory, because rename is only atomic
// within a filesystem: falling back to os.TempDir would make the rename a cross-device
// copy on any real deployment, where PGDATA is a PVC and /tmp is the container's
// overlay -- which is exactly the non-atomic write this package exists to prevent.
func TestTempFileIsCreatedBesideTheTarget(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pgdata")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "postgresql.conf")
	// A directory the write cannot create into: if the temp file went anywhere else,
	// this would succeed.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this relies on")
	}
	if err := WriteString(path, "x\n", 0o600); err == nil {
		t.Error("the temp file was not created in the target's own directory, so the rename would cross filesystems")
	}
}

// A write into a directory that does not exist must fail rather than create it: the
// callers pass paths under PGDATA, and a missing parent means the volume is not mounted
// where the agent thinks it is -- silently materialising the tree would hide that.
func TestWriteDoesNotCreateMissingParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "postgresql.conf")
	if err := WriteString(path, "x\n", 0o600); err == nil {
		t.Fatal("expected an error for a missing parent directory")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Error("the missing parent directory was created")
	}
}

// Zero bytes is a legitimate content: standby.signal is written exactly this way, and
// its presence -- not its content -- is what tells PostgreSQL to start in recovery.
func TestWriteAcceptsEmptyContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "standby.signal")
	if err := WriteString(path, "", 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("standby.signal was not created: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("size = %d, want 0", fi.Size())
	}
}
