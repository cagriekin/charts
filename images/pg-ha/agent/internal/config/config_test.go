package config

import (
	"fmt"
	"path/filepath"
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

// The jitter bound (#298 review): client-go's NewLeaderElector requires
// RenewDeadline > 1.2 x RetryPeriod, and rejecting it there is silent and
// unretryable -- so Load must catch it at boot.
func TestLoadRejectsRenewDeadlineWithinRetryJitter(t *testing.T) {
	m := fullEnv()
	m["LEASE_DURATION"] = "15s"
	m["RENEW_DEADLINE"] = "5s"
	m["RETRY_PERIOD"] = "4500ms" // ordering holds, but 5s <= 1.2 x 4.5s
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "jitter") {
		t.Errorf("expected jitter-bound validation error, got %v", err)
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

// #298 security review: role names are interpolated raw into pg_hba.conf, so a value with
// whitespace/newline would split or inject a rule. Reject at boot, symmetric with POD_CIDR.
func TestLoadRejectsWhitespaceInRoleNames(t *testing.T) {
	for _, key := range []string{"REPMGR_USER", "POSTGRES_USER", "MONITORING_USER"} {
		m := fullEnv()
		m["POSTGRES_USER"] = "app"
		m[key] = "evil all 0.0.0.0/0 trust\nhost"
		_, err := Load(getter(m))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s with a newline must fail validation, got %v", key, err)
		}
	}
}

// The pg_hba user column is structured, and the two halves of that structure have
// DIFFERENT scopes: `,` and `"` are meaningful anywhere in the token, while `+role` and
// `@file` are reserved only in the first position. Anchoring matters both ways -- an
// unanchored ban rejects `app@corp`, a legal role name that arrives by secretKeyRef and
// would crash-loop every pod at boot, and a missing ban lets `all` or `a,b` silently
// widen which rules match.
func TestLoadRoleNameStructureIsAnchoredWhereItShouldBe(t *testing.T) {
	rejected := []string{"all", "ALL", "a,b", `ab"cd`, "+role", "@file"}
	for _, key := range []string{"REPMGR_USER", "POSTGRES_USER", "MONITORING_USER"} {
		for _, v := range rejected {
			m := fullEnv()
			m["POSTGRES_USER"] = "app"
			m[key] = v
			if _, err := Load(getter(m)); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q must fail validation, got %v", key, v, err)
			}
		}
		for _, v := range []string{"app@corp", "team+ops", "user@host.example"} {
			m := fullEnv()
			m["POSTGRES_USER"] = "app"
			m[key] = v
			if _, err := Load(getter(m)); err != nil {
				t.Errorf("%s=%q names exactly itself in pg_hba and must be allowed, got %v", key, v, err)
			}
		}
	}
}

func TestLoadAllowsEmptyMonitoringUser(t *testing.T) {
	m := fullEnv()
	m["POSTGRES_USER"] = "app"
	delete(m, "MONITORING_USER") // "" means no monitoring rule, must not error
	if _, err := Load(getter(m)); err != nil {
		t.Errorf("empty MONITORING_USER must be allowed, got %v", err)
	}
}

// #287: MECHANISM is an enum with a default. Absent must mean repmgr so an existing release
// and an older env-less image keep their behaviour; an unrecognised value must fail at boot
// rather than fall through to whichever branch the factory defaults to.
func TestMechanismDefaultsToNative(t *testing.T) {
	m := fullEnv()
	delete(m, "MECHANISM")
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("MECHANISM must be optional: %v", err)
	}
	if c.Mechanism != MechanismNative {
		t.Errorf("absent MECHANISM must default to %q, got %q", MechanismNative, c.Mechanism)
	}
}

func TestMechanismRepmgrIsRejectedByName(t *testing.T) {
	// Rejected with a message that says the mechanism was REMOVED, not that the value was
	// mistyped (#294): a pod carrying it is running a chart that still asks for repmgr, and
	// conflating the two sends the operator looking for a spelling mistake.
	m := fullEnv()
	m["MECHANISM"] = MechanismRepmgr
	_, err := Load(getter(m))
	if err == nil {
		t.Fatal("MECHANISM=repmgr must be rejected")
	}
	for _, want := range []string{"removed in chart 2.0.0", "#294", "#292", MechanismNative} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %v", want, err)
		}
	}
}

func TestMechanismUnrecognisedValueIsRejected(t *testing.T) {
	m := fullEnv()
	m["MECHANISM"] = "patroni"
	if _, err := Load(getter(m)); err == nil {
		t.Fatal("an unrecognised MECHANISM must be rejected")
	}
}

func TestMechanismAcceptsNative(t *testing.T) {
	m := fullEnv()
	m["MECHANISM"] = MechanismNative
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("MECHANISM=native must load: %v", err)
	}
	if c.Mechanism != MechanismNative {
		t.Errorf("want %q, got %q", MechanismNative, c.Mechanism)
	}
}

func TestMechanismRejectsUnknown(t *testing.T) {
	m := fullEnv()
	m["MECHANISM"] = "patroni"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "MECHANISM") {
		t.Errorf("an unrecognised MECHANISM must fail at boot naming the var, got %v", err)
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

// --- #269: PG_MAJOR / PGBindir ---

// An image built before the PG_MAJOR build arg existed sets no PG_MAJOR, and every
// such image was PostgreSQL 18. Defaulting (rather than erroring) is what keeps the
// agent working when the chart pins an older repmgr image.
func TestLoadPGMajorDefaultsTo18(t *testing.T) {
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.PGMajor != "18" {
		t.Errorf("PGMajor should default to 18, got %q", c.PGMajor)
	}
	if got, want := c.PGBindir(), "/usr/lib/postgresql/18/bin"; got != want {
		t.Errorf("PGBindir() = %q, want %q", got, want)
	}
}

func TestLoadPGMajorOverride(t *testing.T) {
	m := fullEnv()
	m["PG_MAJOR"] = " 17 " // image ENV values are trimmed
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.PGMajor != "17" {
		t.Errorf("PGMajor = %q, want 17", c.PGMajor)
	}
	if got, want := c.PGBindir(), "/usr/lib/postgresql/17/bin"; got != want {
		t.Errorf("PGBindir() = %q, want %q", got, want)
	}
}

// The value is joined into an exec path, so a non-numeric major must fail at config
// load with the variable named -- not as an exec error deep in a reconcile tick.
func TestLoadRejectsNonNumericPGMajor(t *testing.T) {
	for _, v := range []string{"17.2", "pg17", "../18", "17-beta"} {
		m := fullEnv()
		m["PG_MAJOR"] = v
		if _, err := Load(getter(m)); err == nil {
			t.Errorf("PG_MAJOR=%q should be rejected", v)
		} else if !strings.Contains(err.Error(), "PG_MAJOR") {
			t.Errorf("PG_MAJOR=%q: error should name PG_MAJOR: %v", v, err)
		}
	}
}

func TestStringIncludesPGMajor(t *testing.T) {
	m := fullEnv()
	m["PG_MAJOR"] = "17"
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(c.String(), "PGMajor:17") {
		t.Errorf("String() should surface the major for startup logs: %s", c.String())
	}
}

// --- Control API (#276) ---

// controlEnv is a fully-configured control API on top of fullEnv.
func controlEnv() map[string]string {
	m := fullEnv()
	m["CONTROL_ENABLED"] = "true"
	m["CONTROL_TLS_CERT"] = "/etc/agent-control-tls/tls.crt"
	m["CONTROL_TLS_KEY"] = "/etc/agent-control-tls/tls.key"
	m["CONTROL_TLS_CA"] = "/etc/agent-control-tls/ca.crt"
	return m
}

// The default install must be untouched: no control listener, and no error for the
// absence of TLS material that is only needed when the API is on.
func TestControlDisabledByDefault(t *testing.T) {
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.ControlEnabled {
		t.Error("ControlEnabled must default to false")
	}
	if c.ControlAddr != ":9201" {
		t.Errorf("ControlAddr = %q, want :9201", c.ControlAddr)
	}
	if c.ControlRestoreEnabled {
		t.Error("ControlRestoreEnabled must default to false")
	}
}

// Enabling the API without mTLS material must fail the boot, naming every missing
// file at once -- never fall back to an unauthenticated mutating port.
func TestControlEnabledRequiresAllTLSMaterial(t *testing.T) {
	m := fullEnv()
	m["CONTROL_ENABLED"] = "true"
	_, err := Load(getter(m))
	if err == nil {
		t.Fatal("control enabled without TLS material must fail")
	}
	for _, want := range []string{"CONTROL_TLS_CERT", "CONTROL_TLS_KEY", "CONTROL_TLS_CA"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
}

// Partial material is the likelier mistake (a `kubectl create secret tls` has no
// ca.crt): it must fail just as loudly as none at all.
func TestControlEnabledRejectsPartialTLSMaterial(t *testing.T) {
	m := controlEnv()
	delete(m, "CONTROL_TLS_CA")
	_, err := Load(getter(m))
	if err == nil {
		t.Fatal("control enabled without a CA must fail: client certs could not be verified")
	}
	if !strings.Contains(err.Error(), "CONTROL_TLS_CA") {
		t.Errorf("error should name CONTROL_TLS_CA: %v", err)
	}
}

func TestControlEnabledValid(t *testing.T) {
	c, err := Load(getter(controlEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.ControlEnabled || c.ControlCAFile == "" {
		t.Errorf("bad parse: %+v", c)
	}
	if len(c.ControlAllowedCNs) != 0 {
		t.Errorf("ControlAllowedCNs should be empty (any cert from the CA): %v", c.ControlAllowedCNs)
	}
}

func TestControlAllowedCNsParsed(t *testing.T) {
	m := controlEnv()
	m["CONTROL_ALLOWED_CNS"] = " ops-admin , , ci-bot "
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.ControlAllowedCNs) != 2 || c.ControlAllowedCNs[0] != "ops-admin" || c.ControlAllowedCNs[1] != "ci-bot" {
		t.Errorf("ControlAllowedCNs = %#v, want [ops-admin ci-bot]", c.ControlAllowedCNs)
	}
}

func TestControlRejectsBadAddr(t *testing.T) {
	m := controlEnv()
	m["CONTROL_ADDR"] = "9201"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "CONTROL_ADDR") {
		t.Errorf("a non host:port CONTROL_ADDR must be rejected: %v", err)
	}
}

func TestControlRestoreRequiresControl(t *testing.T) {
	m := fullEnv()
	m["CONTROL_RESTORE_ENABLED"] = "true"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "CONTROL_RESTORE_ENABLED requires CONTROL_ENABLED") {
		t.Errorf("restore without the control API must be rejected: %v", err)
	}
}

// The restore authz list has no "allow all" reading: enabling restore without
// naming callers must deny everyone, reported at boot.
func TestControlRestoreRequiresAllowedCNs(t *testing.T) {
	m := controlEnv()
	m["CONTROL_RESTORE_ENABLED"] = "true"
	m["CONTROL_RESTORE_CRONJOB"] = "pg-pgbackrest-restore"
	m["CONTROL_RESTORE_JOB_NAME"] = "pg-pgbackrest-restore-api"
	m["CONTROL_RESTORE_POD_ORDINAL"] = "0"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "CONTROL_RESTORE_ALLOWED_CNS") {
		t.Errorf("restore without an allowlist must be rejected: %v", err)
	}
}

func TestControlRestoreValid(t *testing.T) {
	m := controlEnv()
	m["CONTROL_RESTORE_ENABLED"] = "true"
	m["CONTROL_RESTORE_ALLOWED_CNS"] = "dba-break-glass"
	m["CONTROL_RESTORE_CRONJOB"] = "pg-pgbackrest-restore"
	m["CONTROL_RESTORE_JOB_NAME"] = "pg-pgbackrest-restore-api"
	m["CONTROL_RESTORE_POD_ORDINAL"] = "0"
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.ControlRestoreEnabled || c.ControlRestoreCronJob == "" || c.ControlRestoreJobName == "" {
		t.Errorf("bad parse: %+v", c)
	}
	if c.ControlRestorePodOrdinal != 0 {
		t.Errorf("ControlRestorePodOrdinal = %d, want 0", c.ControlRestorePodOrdinal)
	}
	if c.ControlRestoreReadPodLogs {
		t.Error("ControlRestoreReadPodLogs must default to false (it widens RBAC)")
	}
}

// The log-tailing flag is the one that adds a namespace-wide `get pods/log` grant,
// so setting it where it cannot apply is named, not ignored.
func TestControlRestoreReadPodLogsRequiresRestore(t *testing.T) {
	m := controlEnv()
	m["CONTROL_RESTORE_READ_POD_LOGS"] = "true"
	_, err := Load(getter(m))
	if err == nil || !strings.Contains(err.Error(), "CONTROL_RESTORE_READ_POD_LOGS") {
		t.Errorf("readPodLogs without restore must be rejected: %v", err)
	}
}

// The startup config log must show the control posture without listing identities.
func TestStringSummarisesControlWithoutCNs(t *testing.T) {
	m := controlEnv()
	m["CONTROL_ALLOWED_CNS"] = "ops-admin"
	c, err := Load(getter(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := c.String()
	for _, want := range []string{"Control:true", "ControlMTLS:true", "ControlAllowedCNs:1"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() should contain %q: %s", want, s)
		}
	}
	if strings.Contains(s, "ops-admin") {
		t.Errorf("String() should summarise the allowlist by count, not spell out identities: %s", s)
	}
}

// #335: the agent needs the operator's TLS INTENT, and it cannot be derived from
// TLS_REQUIRE_SSL -- `require` defaults to false, so an absent variable and a deliberate
// `require: false` are indistinguishable. TLS_ENABLED is that separate signal, and it must
// default to off so a release without TLS behaves exactly as it did before.
func TestLoadReadsTLSEnabledIndependentlyOfRequire(t *testing.T) {
	e := fullEnv()
	e["TLS_ENABLED"] = "true"
	c, err := Load(getter(e))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.TLSEnabled {
		t.Error("TLS_ENABLED=true must set TLSEnabled")
	}
	if c.TLSRequireSSL {
		t.Error("TLSRequireSSL must stay false: TLS being on says nothing about clients being forced onto it")
	}
}

func TestLoadDefaultsTLSEnabledOff(t *testing.T) {
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.TLSEnabled {
		t.Error("TLSEnabled must default to false when the chart mounted no server TLS")
	}
}

// #298 review: zero and negative durations PARSE cleanly, and every cross-field
// ordering check in Load is gated on `> 0` -- so without an explicit sign check the
// most broken values are exactly the ones that skip validation. RECONCILE_INTERVAL=0s
// reached time.NewTicker in run(), which panics on a non-positive interval: PID 1
// crash-loops on every pod at once, over a value the chart renders happily.
func TestLoadRejectsNonPositiveDurations(t *testing.T) {
	for _, c := range []struct{ key, val string }{
		{"RECONCILE_INTERVAL", "0s"},
		{"RECONCILE_INTERVAL", "-5s"},
		{"LEASE_DURATION", "0s"},
		{"LEASE_DURATION", "-1s"},
		{"RENEW_DEADLINE", "0s"},
		{"RETRY_PERIOD", "-2s"},
	} {
		m := fullEnv()
		m[c.key] = c.val
		_, err := Load(getter(m))
		if err == nil {
			t.Errorf("%s=%s was accepted", c.key, c.val)
			continue
		}
		// The message has to name the offending key AND say what was wrong with it:
		// "0s" parses, so a bare "invalid duration" would send an operator hunting a
		// typo that is not there.
		if !strings.Contains(err.Error(), c.key) || !strings.Contains(err.Error(), "POSITIVE") {
			t.Errorf("%s=%s: error should name the key and require a positive value: %v", c.key, c.val, err)
		}
	}
}

// A non-positive duration must not ALSO be reported as an ordering violation: the
// cross-field checks compare against a value that was never valid, and a second
// derived complaint just buries the real one.
func TestLoadNonPositiveDurationIsReportedOnce(t *testing.T) {
	m := fullEnv()
	m["LEASE_DURATION"] = "-1s"
	_, err := Load(getter(m))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "RENEW_DEADLINE <") || strings.Contains(err.Error(), "LEASE_DURATION >= 5s") {
		t.Errorf("a negative LEASE_DURATION should not also trip the ordering/floor checks: %v", err)
	}
}

// PGBindir and PGLibdir both derive from PG_MAJOR, and they are used for opposite
// judgements: the first says "run THIS image's postgres", the second answers "is the
// library this data directory asks for genuinely absent from this image" (#293). A
// wrong path makes the first fail loudly and the second fail SILENTLY -- it would
// report every library as missing, or none.
func TestVersionedPathsTrackPGMajor(t *testing.T) {
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PGMajor == "" {
		t.Fatal("PG_MAJOR did not load; the paths below would be meaningless")
	}
	wantBin := "/usr/lib/postgresql/" + c.PGMajor + "/bin"
	if c.PGBindir() != wantBin {
		t.Errorf("PGBindir() = %q, want %q", c.PGBindir(), wantBin)
	}
	wantLib := "/usr/lib/postgresql/" + c.PGMajor + "/lib"
	if c.PGLibdir() != wantLib {
		t.Errorf("PGLibdir() = %q, want %q", c.PGLibdir(), wantLib)
	}
	if c.PGBindir() == c.PGLibdir() {
		t.Error("binaries and modules live in different directories; conflating them makes the #293 diagnostic meaningless")
	}
}

// The restore status record lives BESIDE PGDATA, not inside it: a restore must never
// write into the directory it is restoring, and the record has to survive on the PVC
// after the Job is gone.
func TestRestoreStatusPathSitsBesidePGDATA(t *testing.T) {
	c, err := Load(getter(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	got := c.RestoreStatusPath()
	if strings.HasPrefix(got, strings.TrimRight(c.PGDATA, "/")+"/") {
		t.Errorf("%q is INSIDE PGDATA %q: a restore would write into the directory it is rewriting", got, c.PGDATA)
	}
	if filepath.Dir(got) != filepath.Dir(c.PGDATA) {
		t.Errorf("%q is not beside PGDATA %q, so it would not survive on the same PVC", got, c.PGDATA)
	}
}

// String is what a startup log line renders. The repmgr password must never appear in
// it -- a past code-scanning alert was exactly this.
func TestStringRedactsTheRepmgrPassword(t *testing.T) {
	m := fullEnv()
	m["REPMGR_PASSWORD"] = "hunter2-do-not-log"
	c, err := Load(getter(m))
	if err != nil {
		t.Fatal(err)
	}
	s := c.String()
	if strings.Contains(s, "hunter2-do-not-log") {
		t.Fatalf("the repmgr password reached the rendered config: %s", s)
	}
	if !strings.Contains(s, "RepmgrPassword:***") {
		t.Errorf("the redaction marker is missing, so a future field could leak unnoticed: %s", s)
	}
	// fmt must route through it: a %v on the struct is the shape a log call takes.
	if v := fmt.Sprintf("%v", c); strings.Contains(v, "hunter2-do-not-log") {
		t.Errorf("%%v bypassed String(): %s", v)
	}
}
