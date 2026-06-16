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
