package config

import (
	"strings"
	"testing"
	"time"
)

func fullEnv() map[string]string {
	return map[string]string{
		"POD_NAME":           "pg-0",
		"NAMESPACE":          "db",
		"LEASE_NAME":         "pg-leader",
		"LEASE_DURATION":     "15s",
		"RENEW_DEADLINE":     "10s",
		"RETRY_PERIOD":       "2s",
		"RECONCILE_INTERVAL": "5s",
		"HEADLESS_SERVICE":   "pg-headless",
		"REPMGR_NODE_COUNT":  "3",
		"MASTER_SERVICE":     "pg",
		"PRIMARY_MARKER":     "pg-primary",
		"POD_SELECTOR":       "app.kubernetes.io/component=postgresql",
		"REPMGR_USER":        "repmgr",
		"REPMGR_DB":          "repmgr",
		"REPMGR_PASSWORD":    "secret",
		"PGDATA":             "/var/lib/postgresql/data/pgdata",
		"DCS_BACKEND":        "kubernetes",
		"SPLIT_BRAIN_ACTION": "log",
		"POD_CIDR":           "10.0.0.0/8",
	}
}

func getter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadValid(t *testing.T) {
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.PodName != "pg-0" || c.NodeCount != 3 || c.LeaseDuration != 15*time.Second {
		t.Errorf("bad parse: %+v", c)
	}
}

func TestLoadReportsAllMissing(t *testing.T) {
	m := fullEnv()
	delete(m, "POD_NAME")
	delete(m, "LEASE_NAME")
	_, err := Load(getter(m))
	if err == nil {
		t.Fatal("expected error for missing vars")
	}
	if !strings.Contains(err.Error(), "POD_NAME") || !strings.Contains(err.Error(), "LEASE_NAME") {
		t.Errorf("error should list all missing vars: %v", err)
	}
}

func TestLoadRejectsInconsistentTimings(t *testing.T) {
	m := fullEnv()
	m["RENEW_DEADLINE"] = "20s" // > LeaseDuration (15s): invalid
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "lease timings") {
		t.Errorf("expected lease-timing validation error, got %v", err)
	}
}

func TestLoadRejectsBadDCSBackend(t *testing.T) {
	m := fullEnv()
	m["DCS_BACKEND"] = "consul"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "DCS_BACKEND") {
		t.Errorf("expected DCS_BACKEND validation error, got %v", err)
	}
}

func TestLoadRejectsBadSplitBrainAction(t *testing.T) {
	m := fullEnv()
	m["SPLIT_BRAIN_ACTION"] = "nuke"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "SPLIT_BRAIN_ACTION") {
		t.Errorf("expected SPLIT_BRAIN_ACTION validation error, got %v", err)
	}
}

func TestLoadEtcdBackendRequiresEndpointsAndPrefix(t *testing.T) {
	m := fullEnv()
	m["DCS_BACKEND"] = "etcd" // ETCD_ENDPOINTS / ETCD_PREFIX not set
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "ETCD_ENDPOINTS") || !strings.Contains(err.Error(), "ETCD_PREFIX") {
		t.Errorf("etcd backend must require ETCD_ENDPOINTS + ETCD_PREFIX, got %v", err)
	}
}

func TestLoadEtcdBackendParsesEndpoints(t *testing.T) {
	m := fullEnv()
	m["DCS_BACKEND"] = "etcd"
	m["ETCD_ENDPOINTS"] = "https://a:2379, https://b:2379 ,https://c:2379"
	m["ETCD_PREFIX"] = "/pg-ha/rel/leader"
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.EtcdEndpoints) != 3 || c.EtcdEndpoints[0] != "https://a:2379" || c.EtcdEndpoints[2] != "https://c:2379" {
		t.Errorf("endpoints not split/trimmed: %#v", c.EtcdEndpoints)
	}
	if c.EtcdPrefix != "/pg-ha/rel/leader" {
		t.Errorf("prefix = %q", c.EtcdPrefix)
	}
}

func TestLoadEtcdBackendRejectsTinyLeaseDuration(t *testing.T) {
	m := fullEnv()
	m["DCS_BACKEND"] = "etcd"
	m["ETCD_ENDPOINTS"] = "https://a:2379"
	m["ETCD_PREFIX"] = "/p"
	// keep the ordering valid (3>2>1) so the etcd-min check is what trips, not ordering
	m["LEASE_DURATION"] = "3s"
	m["RENEW_DEADLINE"] = "2s"
	m["RETRY_PERIOD"] = "1s"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "LEASE_DURATION >= 5s") {
		t.Errorf("etcd backend must reject a sub-5s LeaseDuration, got %v", err)
	}
}

func TestLoadKubernetesBackendIgnoresEtcdVars(t *testing.T) {
	// In kubernetes mode the etcd vars are neither required nor read.
	c, err := Load(getter(fullEnv())) // DCS_BACKEND=kubernetes
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.EtcdEndpoints) != 0 || c.EtcdPrefix != "" {
		t.Errorf("etcd config should be empty in kubernetes mode: %#v", c.EtcdEndpoints)
	}
}

func TestLoadRequiresPodCIDR(t *testing.T) {
	m := fullEnv()
	delete(m, "POD_CIDR")
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "POD_CIDR") {
		t.Errorf("POD_CIDR must be required (agent owns the hardened pg_hba), got %v", err)
	}
}

func TestLoadParsesPgHbaRules(t *testing.T) {
	m := fullEnv()
	m["POSTGRESQL_PGHBA"] = "host all admin 1.2.3.0/24 scram-sha-256\n\n  host all bob 5.6.7.0/24 reject  "
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.PgHbaRules) != 2 || c.PgHbaRules[0] != "host all admin 1.2.3.0/24 scram-sha-256" || c.PgHbaRules[1] != "host all bob 5.6.7.0/24 reject" {
		t.Errorf("pgHba rules not split/trimmed (blank dropped): %#v", c.PgHbaRules)
	}
	if c.PgHbaPeerCIDR != "10.0.0.0/8" {
		t.Errorf("PgHbaPeerCIDR = %q", c.PgHbaPeerCIDR)
	}
}

func TestStringRedactsPassword(t *testing.T) {
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	s := c.String()
	if strings.Contains(s, "secret") {
		t.Errorf("String() leaked the password: %s", s)
	}
	if !strings.Contains(s, "RepmgrPassword:***") {
		t.Errorf("String() should mask the password: %s", s)
	}
}

// --- #110: client-TLS config fields ---

func TestLoadTLSFieldsDefaultOff(t *testing.T) {
	// fullEnv() sets no TLS vars -> every #110 field is off/empty (existing installs
	// are unchanged; no "missing" error).
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSRequireSSL || c.TLSClientCertAuth {
		t.Errorf("TLS booleans must default false: require=%v mtls=%v", c.TLSRequireSSL, c.TLSClientCertAuth)
	}
	if c.PostgresUser != "" || c.MonitoringUser != "" {
		t.Errorf("user exemptions must default empty: postgres=%q monitoring=%q", c.PostgresUser, c.MonitoringUser)
	}
}

func TestLoadTLSFieldsParsed(t *testing.T) {
	m := fullEnv()
	m["TLS_REQUIRE_SSL"] = "true"
	m["TLS_CLIENT_CERT_AUTH"] = "true"
	m["POSTGRES_USER"] = "  postgres  " // trimmed
	m["MONITORING_USER"] = "monitoring"
	c, err := Load(getter(m))
	if err != nil {
		t.Fatal(err)
	}
	if !c.TLSRequireSSL || !c.TLSClientCertAuth {
		t.Errorf("expected both TLS booleans true: %+v", c)
	}
	if c.PostgresUser != "postgres" {
		t.Errorf("POSTGRES_USER must be trimmed: %q", c.PostgresUser)
	}
	if c.MonitoringUser != "monitoring" {
		t.Errorf("MONITORING_USER mismatch: %q", c.MonitoringUser)
	}
}

func TestLoadBoolEnvVariants(t *testing.T) {
	truthy := []string{"true", "TRUE", "True", "1", "yes", "YES", " true "}
	falsy := []string{"", "false", "0", "no", "off", "garbage", "2"}
	for _, v := range truthy {
		m := fullEnv()
		m["TLS_REQUIRE_SSL"] = v
		c, err := Load(getter(m))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if !c.TLSRequireSSL {
			t.Errorf("TLS_REQUIRE_SSL=%q should parse true", v)
		}
	}
	for _, v := range falsy {
		m := fullEnv()
		m["TLS_REQUIRE_SSL"] = v
		c, err := Load(getter(m))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if c.TLSRequireSSL {
			t.Errorf("TLS_REQUIRE_SSL=%q should parse false", v)
		}
	}
}
