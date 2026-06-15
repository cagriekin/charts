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
