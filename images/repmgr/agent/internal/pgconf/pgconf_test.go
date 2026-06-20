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

func TestAssemblePgHbaRequireSSL(t *testing.T) {
	out := AssemblePgHba(PgHbaOptions{
		ReplicationUser: "repmgr",
		PeerCIDR:        "10.0.0.0/8",
		RequireSSL:      true,
	})
	// The peer-CIDR client rule is hostssl (reject non-TLS).
	if !strings.Contains(out, "hostssl     all           all        10.0.0.0/8        scram-sha-256") {
		t.Errorf("require: peer-CIDR client rule must be hostssl:\n%s", out)
	}
	// Loopback and replication stay plain `host` (never hostssl) -- replication is plaintext (#110).
	if !strings.Contains(out, "host        all           all        127.0.0.1/32") {
		t.Errorf("require: loopback must stay plain host:\n%s", out)
	}
	if !strings.Contains(out, "host        replication   repmgr        10.0.0.0/8        scram-sha-256") {
		t.Errorf("require: replication rule must stay plain host:\n%s", out)
	}
	if strings.Contains(out, "clientcert") {
		t.Errorf("require without mTLS must not add clientcert:\n%s", out)
	}
}

func TestAssemblePgHbaClientCertAuthExemptsInternalUsers(t *testing.T) {
	out := AssemblePgHba(PgHbaOptions{
		ReplicationUser: "repmgr",
		PeerCIDR:        "10.0.0.0/8",
		RequireSSL:      true,
		ClientCertAuth:  true,
		PostgresUser:    "postgres",
		MonitoringUser:  "monitoring",
	})
	// Internal users get hostssl WITHOUT clientcert (password over TLS).
	for _, u := range []string{"repmgr", "postgres", "monitoring"} {
		line := "hostssl     all           " + u + "        10.0.0.0/8        scram-sha-256"
		if !strings.Contains(out, line) {
			t.Errorf("mTLS: internal user %q must be exempt from clientcert:\n%s", u, out)
		}
	}
	// The app catch-all requires a client cert.
	if !strings.Contains(out, "hostssl     all           all        10.0.0.0/8        scram-sha-256 clientcert=verify-ca") {
		t.Errorf("mTLS: app catch-all must require clientcert=verify-ca:\n%s", out)
	}
	// Exemptions must appear ABOVE the clientcert catch-all (first match wins in pg_hba).
	exIdx := strings.Index(out, "hostssl     all           postgres")
	catchIdx := strings.Index(out, "clientcert=verify-ca")
	if exIdx < 0 || catchIdx < 0 || exIdx > catchIdx {
		t.Errorf("exemptions must precede the clientcert catch-all (ex=%d catch=%d):\n%s", exIdx, catchIdx, out)
	}
	// Replication stays plaintext even under mTLS.
	if !strings.Contains(out, "host        replication   repmgr        10.0.0.0/8        scram-sha-256") {
		t.Errorf("mTLS: replication rule must stay plain host:\n%s", out)
	}
}

func TestAssemblePgHbaClientCertAuthSkipsEmptyMonitoringUser(t *testing.T) {
	out := AssemblePgHba(PgHbaOptions{
		ReplicationUser: "repmgr",
		PeerCIDR:        "10.0.0.0/8",
		ClientCertAuth:  true,
		PostgresUser:    "postgres",
		// MonitoringUser empty -> no monitoring exemption rule emitted.
	})
	if strings.Contains(out, "monitoring") {
		t.Errorf("empty MonitoringUser must not emit a monitoring exemption:\n%s", out)
	}
	if !strings.Contains(out, "clientcert=verify-ca") {
		t.Errorf("mTLS catch-all must still require clientcert:\n%s", out)
	}
}

func TestAssemblePgHbaDefaultUnchangedByTLSFields(t *testing.T) {
	// TLS off (zero-value options): byte-identical to the pre-#110 hardened default.
	out := AssemblePgHba(PgHbaOptions{ReplicationUser: "repmgr", PeerCIDR: "10.0.0.0/8"})
	if !strings.Contains(out, "host        all           all        10.0.0.0/8        scram-sha-256") {
		t.Errorf("default must keep the plain host catch-all:\n%s", out)
	}
	if strings.Contains(out, "hostssl") || strings.Contains(out, "clientcert") {
		t.Errorf("TLS off must not emit hostssl/clientcert:\n%s", out)
	}
}

func TestAssemblePgHbaClientCertAuthOnlyForcesHostssl(t *testing.T) {
	// clientCertAuth WITHOUT require (require=false): the switch checks ClientCertAuth
	// first, so it still emits the full mTLS rule set -- there is NO plaintext host
	// catch-all. This is the combination behind the cross-component guard fix (the
	// exporter/pgpool sslmode guards must fire on clientCertAuth, not just require).
	out := AssemblePgHba(PgHbaOptions{
		ReplicationUser: "repmgr",
		PeerCIDR:        "10.0.0.0/8",
		ClientCertAuth:  true,
		RequireSSL:      false, // explicitly off
		PostgresUser:    "postgres",
		MonitoringUser:  "monitoring",
	})
	if !strings.Contains(out, "hostssl     all           all        10.0.0.0/8        scram-sha-256 clientcert=verify-ca") {
		t.Errorf("clientCertAuth alone must emit the clientcert catch-all:\n%s", out)
	}
	// No plaintext peer-CIDR catch-all may survive (else non-TLS clients slip through).
	if strings.Contains(out, "host        all           all        10.0.0.0/8") {
		t.Errorf("clientCertAuth must not leave a plaintext host catch-all:\n%s", out)
	}
}

func TestAssemblePgHbaRequireAndClientCertAuthMatchesClientCertOnly(t *testing.T) {
	// require=true is subsumed by clientCertAuth=true: the rule set is identical whether
	// require is also set, so the two flags never produce conflicting/duplicate rules.
	base := PgHbaOptions{ReplicationUser: "repmgr", PeerCIDR: "10.0.0.0/8", ClientCertAuth: true, PostgresUser: "postgres"}
	withReq := base
	withReq.RequireSSL = true
	if AssemblePgHba(base) != AssemblePgHba(withReq) {
		t.Errorf("require+clientCertAuth must equal clientCertAuth-only:\n--- cca ---\n%s\n--- cca+req ---\n%s", AssemblePgHba(base), AssemblePgHba(withReq))
	}
}

func TestAssemblePgHbaEmptyPostgresUserMTLS(t *testing.T) {
	// Empty PostgresUser -> no postgres exemption line and no malformed empty-user rule;
	// the repmgr exemption and the clientcert catch-all are still emitted.
	out := AssemblePgHba(PgHbaOptions{
		ReplicationUser: "repmgr",
		PeerCIDR:        "10.0.0.0/8",
		ClientCertAuth:  true,
		// PostgresUser + MonitoringUser empty
	})
	if !strings.Contains(out, "hostssl     all           repmgr        10.0.0.0/8        scram-sha-256") {
		t.Errorf("repmgr exemption must still be present:\n%s", out)
	}
	if !strings.Contains(out, "clientcert=verify-ca") {
		t.Errorf("clientcert catch-all must still be present:\n%s", out)
	}
	// A double-space gap (empty username spliced into the format) would be a malformed rule.
	if strings.Contains(out, "hostssl     all                     10.0.0.0/8") {
		t.Errorf("empty PostgresUser must not splice a blank-username rule:\n%s", out)
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
