package pgconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssemblePgHbaIsHardened(t *testing.T) {
	out := AssemblePgHba(PgHbaOptions{
		ReplicationUser: "repmgr",
		PeerCIDR:        "10.0.0.0/8",
		ExtraRules:      []string{"host all readonly 10.0.0.0/8 scram-sha-256"},
	})
	if strings.Contains(out, "0.0.0.0/0") {
		t.Errorf("must not contain the 0.0.0.0/0 catch-all:\n%s", out)
	}
	if strings.Contains(out, "md5") {
		t.Errorf("must be SCRAM-only, found md5:\n%s", out)
	}
	if !strings.Contains(out, "host        replication   repmgr        10.0.0.0/8        scram-sha-256") {
		t.Errorf("missing replication rule:\n%s", out)
	}
	// User rule must appear ABOVE the network catch-all (#144 ordering).
	userIdx := strings.Index(out, "readonly")
	catchIdx := strings.Index(out, "host        all           all        10.0.0.0/8")
	if userIdx < 0 || catchIdx < 0 || userIdx > catchIdx {
		t.Errorf("user rule must precede the network catch-all (user=%d catch=%d):\n%s", userIdx, catchIdx, out)
	}
}

func TestEnsureConfdIncludeIdempotentToggle(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "postgresql.conf")
	if err := os.WriteFile(conf, []byte("shared_buffers = '128MB'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inc := "/etc/postgresql/conf.d"
	managed := "include_dir = '/etc/postgresql/conf.d'"

	// Enable twice: line present exactly once (idempotent).
	for i := 0; i < 2; i++ {
		if err := EnsureConfdInclude(conf, inc, true); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(conf)
	if n := strings.Count(string(b), managed); n != 1 {
		t.Errorf("managed include count = %d, want 1:\n%s", n, b)
	}
	if !strings.Contains(string(b), "shared_buffers") {
		t.Errorf("existing config must be preserved:\n%s", b)
	}

	// Disable: line removed.
	if err := EnsureConfdInclude(conf, inc, false); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(conf)
	if strings.Contains(string(b), managed) {
		t.Errorf("managed include should be removed when disabled:\n%s", b)
	}
}
