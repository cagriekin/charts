// Package config loads and validates the agent's configuration from the
// environment at boot. Per the fail-fast rule, every required variable is
// validated up front and a single error lists everything missing/invalid; the
// agent terminates rather than starting half-configured.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultPGMajor is assumed when PG_MAJOR is unset -- an image built before the
// PG_MAJOR build arg existed, which was always PostgreSQL 18.
const defaultPGMajor = "18"

// The HA mechanism the agent drives (#287). Optional -- absent means MechanismNative, which
// is the only implementation since #294 deleted mechanism.Repmgr.
//
// MechanismRepmgr is kept as a named constant purely so an explicit `MECHANISM=repmgr` can be
// REJECTED with a message that names the migration, rather than falling through the
// unrecognised-value path and reading as a typo. A pod carrying that value is running a
// release whose chart still sets it, and the operator needs to know the mechanism was removed
// rather than mistyped.
const (
	MechanismRepmgr = "repmgr"
	MechanismNative = "native"
)

// Config is the validated agent configuration.
type Config struct {
	PodName   string // this pod's name — the Lease holder identity
	Namespace string

	LeaseName         string
	LeaseDuration     time.Duration
	RenewDeadline     time.Duration
	RetryPeriod       time.Duration
	ReconcileInterval time.Duration

	HeadlessService string // for building peer FQDNs <pod>.<headless>
	NodeCount       int    // total pods (replicaCount+1) for peer enumeration
	MasterService   string // write Service whose selector the agent patches
	MarkerName      string // durable primary-marker ConfigMap
	PodSelector     string // label selector for the postgresql pods (pg-role labeling)

	RepmgrUser     string
	RepmgrDB       string
	RepmgrPassword string
	PGDATA         string

	// PGMajor is the PostgreSQL major bundled in the image, from the PG_MAJOR env the
	// Dockerfile sets from its build arg (#269). Optional: absent means "18", so an
	// older env-less image keeps working. The agent locates postgres/pg_controldata
	// under the versioned bindir it implies, which is why a PG17 image must announce
	// itself here rather than the agent assuming a major.
	PGMajor string

	DCSBackend string // "kubernetes" | "etcd"

	// Mechanism selects the replication mechanics the agent drives. MechanismNative
	// (pg_ctl / pg_basebackup / pg_rewind directly) is the only implementation since #294;
	// the field and the Mechanism interface behind it survive because that seam is what made
	// the repmgr-to-native migration survivable one method at a time.
	Mechanism string

	// pg_hba assembly (agent mode owns pg_hba: hardened, SCRAM-only, no 0.0.0.0/0 md5).
	PgHbaPeerCIDR string   // POD_CIDR: the trusted pod network for SCRAM rules
	PgHbaRules    []string // POSTGRESQL_PGHBA: user rules, placed above the catch-alls

	// Client TLS pg_hba (issue #110). All optional; default off (TLS disabled).
	//
	// TLSEnabled is the operator's INTENT (postgresql.tls.enabled), carried separately from
	// TLSRequireSSL because the two answer different questions and #335 needs the first one.
	// Presence of TLS_REQUIRE_SSL cannot stand in for it: `require` defaults to false, so an
	// absent variable and a deliberate `require: false` produce the identical value, and the
	// agent could not tell "TLS is off" from "TLS is on but not mandatory for clients".
	TLSEnabled        bool   // TLS_ENABLED: the chart mounted server TLS; `ssl` must end up on
	TLSRequireSSL     bool   // TLS_REQUIRE_SSL: peer-CIDR client rule -> hostssl
	TLSClientCertAuth bool   // TLS_CLIENT_CERT_AUTH: app users need a client cert (mTLS)
	PostgresUser      string // POSTGRES_USER: superuser, exempt from clientcert
	MonitoringUser    string // MONITORING_USER: monitoring user, exempt; "" when disabled

	// md5->scram managed-user re-hash on promotion/boot-primary (#199). When
	// MigrateLegacyMd5Users is set the agent re-hashes any managed user whose stored
	// password is still md5 to scram-sha-256 via local psql; it needs the plaintext
	// superuser/repmgr passwords to ALTER ... PASSWORD. All optional -- absent means
	// the re-hash is skipped (md5 hashes keep authenticating via the md5 pg_hba line).
	// The passwords are already on the postgresql container env, so no new secret is
	// exposed; only the boolean flag env is new.
	MigrateLegacyMd5Users bool   // MIGRATE_LEGACY_MD5_USERS
	PostgresPassword      string // POSTGRES_PASSWORD (superuser, for the re-hash)
	PostgresDB            string // POSTGRES_DB (psql -d target for the re-hash)

	// Cascading replication (issue #29). Optional; default off (every standby follows
	// the primary, byte-stable). When on, a standby may follow another standby to
	// offload the primary's WAL senders, with a safe fallback to the primary.
	CascadeReplication bool // CASCADE_REPLICATION

	// Logical failover slot sync (#308), PG17+. Optional; default off. When on, the
	// primary reconciles synchronized_standby_slots to the current standby's physical
	// slot(s) on every tick it serves, so a logical failover slot can be synced across
	// promote without a full resync. Physical replication is unaffected either way.
	SyncReplicationSlots bool // SYNC_REPLICATION_SLOTS

	// etcd backend (required only when DCSBackend == "etcd"). TLS is optional
	// (all-or-none, enforced by the dcs layer).
	EtcdEndpoints []string
	EtcdPrefix    string
	EtcdCertFile  string
	EtcdKeyFile   string
	EtcdCAFile    string

	// Control API (#276). Optional and OFF by default: absent means the agent serves
	// only the read-only :9200 surface, exactly as before. When on, mTLS is mandatory
	// (there is no password/token mode and no plaintext mode) -- so all three of
	// cert/key/CA are required together and the agent refuses to boot without them
	// rather than opening an unauthenticated mutating port.
	ControlEnabled  bool
	ControlAddr     string // host:port, default :9201; never the metrics port
	ControlCertFile string
	ControlKeyFile  string
	ControlCAFile   string
	// ControlAllowedCNs, when non-empty, restricts control access to client certs
	// whose subject CN is listed. Empty means "any cert signed by ControlCAFile",
	// which is already a closed set (the operator issues the certs).
	ControlAllowedCNs []string

	// Restore-triggering (#276 phase 3) is a SEPARATE opt-in from the rest of the
	// control API, because it is the one verb whose RBAC grant (create jobs, which
	// cannot be resourceName-scoped) is a namespace-wide escalation primitive.
	ControlRestoreEnabled bool
	// ControlRestoreAllowedCNs is the restore-specific authz verb: a client must be
	// listed here to trigger a restore, over and above passing mTLS. Required (and
	// enforced non-empty) when ControlRestoreEnabled, so enabling restore without
	// naming who may call it denies everyone instead of allowing everyone.
	ControlRestoreAllowedCNs []string
	// ControlRestoreCronJob is the chart's suspended restore CronJob, whose
	// jobTemplate the agent clones verbatim -- the same object and the same semantics
	// as `kubectl create job --from=cronjob/<name>`.
	ControlRestoreCronJob string
	// ControlRestoreJobName is the deterministic name of the Job the agent creates.
	// Deterministic (not generateName) so the RBAC get/delete grants can be
	// resourceName-scoped to exactly this one object.
	ControlRestoreJobName string
	// ControlRestorePodOrdinal is the ordinal whose PVC the rendered CronJob restores
	// into. The API accepts a podOrdinal only to CONFIRM it (409 on mismatch): which
	// volume gets overwritten is baked into the rendered Job and stays a values/git
	// decision, never an HTTP-body one.
	ControlRestorePodOrdinal int
	// ControlRestoreReadPodLogs enables live file-copy progress by tailing the restore
	// pod's log, which needs a namespace-wide `get pods/log` grant (the Job's pod name
	// is generated, so it cannot be resourceName-scoped). Off by default: logs are
	// where other workloads leak secrets, and the progress is only observable while
	// some agent pod is still running.
	ControlRestoreReadPodLogs bool

	// pgBackRest, from the env the chart already sets for the archive_command. Used by
	// the control API's read-only backup routes (`pgbackrest info`, restore outcome);
	// absent means this release has no repository, and those routes say so.
	PGBackrestEnabled bool
	PGBackrestStanza  string
}

// RestoreStatusPath is where the chart's restore.sh records the outcome of the last
// restore: beside PGDATA, not inside it, so a restore never writes into the directory
// it is restoring and the record survives on the PVC after the Job is gone.
func (c Config) RestoreStatusPath() string {
	return filepath.Join(filepath.Dir(c.PGDATA), "pgbackrest-restore.status")
}

// defaultControlAddr is the control API's listen address. Deliberately NOT the
// metrics port: :9200 stays observe-only so a NetworkPolicy can admit scrapers
// there without ever admitting them to a mutating surface.
const defaultControlAddr = ":9201"

type loader struct {
	get     func(string) string
	missing []string
	invalid []string
}

func (l *loader) str(key string) string {
	v := l.get(key)
	if v == "" {
		l.missing = append(l.missing, key)
	}
	return v
}

func (l *loader) dur(key string) time.Duration {
	v := l.get(key)
	if v == "" {
		l.missing = append(l.missing, key)
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.invalid = append(l.invalid, fmt.Sprintf("%s=%q (%v)", key, v, err))
	}
	return d
}

// boolEnv parses an optional boolean env value (true/1/yes, case-insensitive);
// anything else (incl. empty/unset) is false.
func boolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// csvList parses an optional comma-separated list, dropping empty entries. Used for
// the control-API cert CN allowlists, where an empty result must stay empty (it is
// the difference between "no restriction" and "deny everyone" at the call site).
func csvList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (l *loader) intv(key string) int {
	v := l.get(key)
	if v == "" {
		l.missing = append(l.missing, key)
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.invalid = append(l.invalid, fmt.Sprintf("%s=%q (%v)", key, v, err))
	}
	return n
}

// Load reads the config using get (os.Getenv in production). It returns an error
// listing every missing/invalid variable so misconfiguration is fixed in one pass.
func Load(get func(string) string) (*Config, error) {
	l := &loader{get: get}
	c := &Config{
		PodName:           l.str("POD_NAME"),
		Namespace:         l.str("NAMESPACE"),
		LeaseName:         l.str("LEASE_NAME"),
		LeaseDuration:     l.dur("LEASE_DURATION"),
		RenewDeadline:     l.dur("RENEW_DEADLINE"),
		RetryPeriod:       l.dur("RETRY_PERIOD"),
		ReconcileInterval: l.dur("RECONCILE_INTERVAL"),
		HeadlessService:   l.str("HEADLESS_SERVICE"),
		NodeCount:         l.intv("REPMGR_NODE_COUNT"),
		MasterService:     l.str("MASTER_SERVICE"),
		MarkerName:        l.str("PRIMARY_MARKER"),
		PodSelector:       l.str("POD_SELECTOR"),
		RepmgrUser:        l.str("REPMGR_USER"),
		RepmgrDB:          l.str("REPMGR_DB"),
		RepmgrPassword:    l.str("REPMGR_PASSWORD"),
		PGDATA:            l.str("PGDATA"),
		DCSBackend:        l.str("DCS_BACKEND"),
		PgHbaPeerCIDR:     l.str("POD_CIDR"),
	}
	// User pg_hba rules (optional), newline-separated; the agent places them above
	// the hardened network catch-alls.
	for _, r := range strings.Split(get("POSTGRESQL_PGHBA"), "\n") {
		if r = strings.TrimSpace(r); r != "" {
			c.PgHbaRules = append(c.PgHbaRules, r)
		}
	}

	// Client TLS pg_hba inputs (issue #110). Optional -- absent/empty means off, so
	// existing installs are unchanged; no "missing" error.
	c.TLSEnabled = boolEnv(get("TLS_ENABLED"))
	c.TLSRequireSSL = boolEnv(get("TLS_REQUIRE_SSL"))
	c.TLSClientCertAuth = boolEnv(get("TLS_CLIENT_CERT_AUTH"))
	c.PostgresUser = strings.TrimSpace(get("POSTGRES_USER"))
	c.MonitoringUser = strings.TrimSpace(get("MONITORING_USER"))

	// md5->scram re-hash inputs (#199). Optional -- the agent runs the re-hash on
	// promotion/boot-primary when enabled; the passwords are already on the postgresql
	// container env (the chart's postStart used them), so no new secret is exposed.
	c.MigrateLegacyMd5Users = boolEnv(get("MIGRATE_LEGACY_MD5_USERS"))
	c.PostgresPassword = get("POSTGRES_PASSWORD")
	c.PostgresDB = strings.TrimSpace(get("POSTGRES_DB"))

	// Cascading replication (issue #29). Optional -- absent/empty means off.
	c.CascadeReplication = boolEnv(get("CASCADE_REPLICATION"))

	// Logical failover slot sync (#308). Optional -- absent/empty means off.
	c.SyncReplicationSlots = boolEnv(get("SYNC_REPLICATION_SLOTS"))

	// PostgreSQL major (#269). Optional -- the image ENV supplies it; default 18 for an
	// older image that predates the build arg. Digits only: the value is joined into the
	// bindir path the agent execs postgres/pg_controldata from, so anything else would
	// surface as a confusing exec failure later instead of a config error now.
	c.PGMajor = strings.TrimSpace(get("PG_MAJOR"))
	if c.PGMajor == "" {
		c.PGMajor = defaultPGMajor
	} else if strings.TrimLeft(c.PGMajor, "0123456789") != "" {
		l.invalid = append(l.invalid, fmt.Sprintf("PG_MAJOR=%q (want digits only, e.g. 17 or 18)", c.PGMajor))
	}

	// HA mechanism (#287, #294). Optional -- absent means native, the only implementation for an
	// existing release. Validated as an enum here rather than discovered at the first
	// promote: an unrecognised value would otherwise silently fall through to whichever
	// branch the factory happens to default to.
	c.Mechanism = strings.TrimSpace(get("MECHANISM"))
	switch c.Mechanism {
	case "":
		// Absent means native now (#294). An older env-less image is not a concern in the other
		// direction: this binary only ships in an image whose chart sets the value or omits it.
		c.Mechanism = MechanismNative
	case MechanismNative:
	case MechanismRepmgr:
		// Named explicitly so the message can say what happened. Falling through to the
		// unrecognised-value branch would read as a typo, when in fact the mechanism was
		// removed and this pod is running a chart that still asks for it.
		l.invalid = append(l.invalid, fmt.Sprintf(
			"MECHANISM=%q was removed in chart 2.0.0 (#294): the repmgr mechanism and its CLI are gone, and %s is now the only one. "+
				"Unset ha.agent.mechanism (or set it to %q) -- an existing repmgr cluster needs the in-place migration first (#292)",
			MechanismRepmgr, MechanismNative, MechanismNative))
	default:
		l.invalid = append(l.invalid, fmt.Sprintf("MECHANISM=%q (want %s)", c.Mechanism, MechanismNative))
	}

	// Cross-field validation that the lease timings are internally consistent
	// (client-go requires LeaseDuration > RenewDeadline > RetryPeriod).
	if c.LeaseDuration > 0 && c.RenewDeadline > 0 && c.RetryPeriod > 0 {
		if !(c.LeaseDuration > c.RenewDeadline && c.RenewDeadline > c.RetryPeriod) {
			l.invalid = append(l.invalid, fmt.Sprintf("lease timings must satisfy LeaseDuration(%s) > RenewDeadline(%s) > RetryPeriod(%s)",
				c.LeaseDuration, c.RenewDeadline, c.RetryPeriod))
		} else if float64(c.RenewDeadline) <= float64(c.RetryPeriod)*1.2 {
			// client-go's NewLeaderElector additionally requires
			// RenewDeadline > RetryPeriod*JitterFactor (1.2) and rejects the elector at
			// construction -- a failure K8sDCS.Run cannot retry, so without this bound a
			// Load-clean config produced an agent that ticks forever yet never contends
			// for leadership (#298 review). Fail the boot with the real bound instead.
			l.invalid = append(l.invalid, fmt.Sprintf("lease timings must satisfy RenewDeadline(%s) > 1.2 x RetryPeriod(%s) (client-go leader-election jitter bound); raise RENEW_DEADLINE or lower RETRY_PERIOD",
				c.RenewDeadline, c.RetryPeriod))
		}
	}
	// Validate enums only when present (an empty value is already a "missing" error).
	if c.DCSBackend != "" && c.DCSBackend != "kubernetes" && c.DCSBackend != "etcd" {
		l.invalid = append(l.invalid, fmt.Sprintf("DCS_BACKEND=%q (want kubernetes|etcd)", c.DCSBackend))
	}
	// POD_CIDR is interpolated raw into pg_hba.conf; a malformed value would corrupt
	// the whole file and fail Postgres start. Validate the CIDR form up front.
	if c.PgHbaPeerCIDR != "" {
		if _, _, err := net.ParseCIDR(c.PgHbaPeerCIDR); err != nil {
			l.invalid = append(l.invalid, fmt.Sprintf("POD_CIDR=%q is not a valid CIDR: %v", c.PgHbaPeerCIDR, err))
		}
	}
	// The role names are interpolated raw into pg_hba.conf lines too (pgconf.AssemblePgHba),
	// so a value carrying whitespace or a newline would split a rule or inject an extra one
	// -- the same class of corruption POD_CIDR is validated against, given the same check
	// here rather than left asymmetric (#298 security review). A PostgreSQL role name has no
	// legitimate need for either. "" is allowed: MONITORING_USER empty means "no monitoring
	// rule", and the missing-required check already covers the others.
	for _, u := range []struct{ name, val string }{
		{"REPMGR_USER", c.RepmgrUser},
		{"POSTGRES_USER", c.PostgresUser},
		{"MONITORING_USER", c.MonitoringUser},
	} {
		if strings.ContainsAny(u.val, " \t\r\n") {
			l.invalid = append(l.invalid, fmt.Sprintf("%s=%q must not contain whitespace: it is written verbatim into pg_hba.conf", u.name, u.val))
		}
	}
	// etcd backend config is required only when DCS_BACKEND=etcd, so a kubernetes
	// install needs none of it. TLS is optional (all-or-none, enforced in dcs).
	if c.DCSBackend == "etcd" {
		if eps := l.get("ETCD_ENDPOINTS"); eps == "" {
			l.missing = append(l.missing, "ETCD_ENDPOINTS")
		} else {
			for _, e := range strings.Split(eps, ",") {
				if e = strings.TrimSpace(e); e != "" {
					c.EtcdEndpoints = append(c.EtcdEndpoints, e)
				}
			}
		}
		if c.EtcdPrefix = strings.TrimSpace(l.get("ETCD_PREFIX")); c.EtcdPrefix == "" {
			l.missing = append(l.missing, "ETCD_PREFIX")
		}
		// The etcd session lease TTL is whole seconds (LeaseDuration maps to it); a
		// sub-5s lease leaves no soft-fence margin given etcd's 1s client deadline
		// granularity + the up-to-1s server reap, and truncation toward an integer TTL
		// would silently degrade it (e.g. 1500ms -> TTL=1). Require >= 5s for etcd.
		if c.LeaseDuration > 0 && c.LeaseDuration < 5*time.Second {
			l.invalid = append(l.invalid, fmt.Sprintf("etcd DCS needs LEASE_DURATION >= 5s (lease TTL is whole seconds), got %s", c.LeaseDuration))
		}
		c.EtcdCertFile = l.get("ETCD_TLS_CERT")
		c.EtcdKeyFile = l.get("ETCD_TLS_KEY")
		c.EtcdCAFile = l.get("ETCD_TLS_CA")
	}

	// Control API (#276). Every field is read unconditionally so a stray
	// CONTROL_RESTORE_* on a control-disabled agent can still be reported as a
	// misconfiguration below, rather than silently ignored.
	c.ControlEnabled = boolEnv(get("CONTROL_ENABLED"))
	c.ControlAddr = strings.TrimSpace(get("CONTROL_ADDR"))
	if c.ControlAddr == "" {
		c.ControlAddr = defaultControlAddr
	}
	c.ControlCertFile = strings.TrimSpace(get("CONTROL_TLS_CERT"))
	c.ControlKeyFile = strings.TrimSpace(get("CONTROL_TLS_KEY"))
	c.ControlCAFile = strings.TrimSpace(get("CONTROL_TLS_CA"))
	c.ControlAllowedCNs = csvList(get("CONTROL_ALLOWED_CNS"))
	c.ControlRestoreEnabled = boolEnv(get("CONTROL_RESTORE_ENABLED"))
	c.ControlRestoreAllowedCNs = csvList(get("CONTROL_RESTORE_ALLOWED_CNS"))
	c.ControlRestoreCronJob = strings.TrimSpace(get("CONTROL_RESTORE_CRONJOB"))
	c.ControlRestoreJobName = strings.TrimSpace(get("CONTROL_RESTORE_JOB_NAME"))
	c.ControlRestoreReadPodLogs = boolEnv(get("CONTROL_RESTORE_READ_POD_LOGS"))
	// pgBackRest presence, already on the container env for the archive_command; the
	// control API's backup routes need it to know whether a repository exists at all.
	c.PGBackrestEnabled = boolEnv(get("PGBACKREST_ENABLED"))
	c.PGBackrestStanza = strings.TrimSpace(get("PGBACKREST_STANZA"))
	if c.ControlEnabled {
		// mTLS is not optional. Named individually so one boot error lists every
		// missing file instead of one-at-a-time discovery across restarts.
		for _, f := range []struct{ key, val string }{
			{"CONTROL_TLS_CERT", c.ControlCertFile},
			{"CONTROL_TLS_KEY", c.ControlKeyFile},
			{"CONTROL_TLS_CA", c.ControlCAFile},
		} {
			if f.val == "" {
				l.missing = append(l.missing, f.key+" (required by CONTROL_ENABLED: the control API is mTLS-only)")
			}
		}
		if _, _, err := net.SplitHostPort(c.ControlAddr); err != nil {
			l.invalid = append(l.invalid, fmt.Sprintf("CONTROL_ADDR=%q is not host:port: %v", c.ControlAddr, err))
		}
	}
	if c.ControlRestoreEnabled {
		if !c.ControlEnabled {
			l.invalid = append(l.invalid, "CONTROL_RESTORE_ENABLED requires CONTROL_ENABLED")
		}
		// Fail closed on the authz list: an empty list denies every client rather than
		// admitting every control client to the most destructive verb in the API.
		if len(c.ControlRestoreAllowedCNs) == 0 {
			l.missing = append(l.missing, "CONTROL_RESTORE_ALLOWED_CNS (required by CONTROL_RESTORE_ENABLED: restore is a separate authz verb, and an empty allowlist denies everyone)")
		}
		if c.ControlRestoreCronJob == "" {
			l.missing = append(l.missing, "CONTROL_RESTORE_CRONJOB")
		}
		if c.ControlRestoreJobName == "" {
			l.missing = append(l.missing, "CONTROL_RESTORE_JOB_NAME")
		}
		c.ControlRestorePodOrdinal = l.intv("CONTROL_RESTORE_POD_ORDINAL")
		if c.ControlRestorePodOrdinal < 0 {
			l.invalid = append(l.invalid, fmt.Sprintf("CONTROL_RESTORE_POD_ORDINAL=%d (want >= 0)", c.ControlRestorePodOrdinal))
		}
	} else if c.ControlRestoreReadPodLogs {
		// Loud rather than ignored: this flag is the one that widens RBAC, so a values
		// file that sets it without enabling restore is a mistake worth naming.
		l.invalid = append(l.invalid, "CONTROL_RESTORE_READ_POD_LOGS requires CONTROL_RESTORE_ENABLED")
	}

	if len(l.missing) > 0 || len(l.invalid) > 0 {
		return nil, fmt.Errorf("config error: missing [%s]; invalid [%s]",
			strings.Join(l.missing, ", "), strings.Join(l.invalid, "; "))
	}
	return c, nil
}

// FromEnv loads the config from the process environment.
func FromEnv() (*Config, error) { return Load(os.Getenv) }

// PGBindir is the versioned directory holding this image's server binaries
// (postgres, pg_ctl, pg_controldata), which Debian installs per major.
func (c Config) PGBindir() string { return "/usr/lib/postgresql/" + c.PGMajor + "/bin" }

// PGLibdir is the versioned directory holding this image's server MODULES -- the
// shared_preload_libraries search path (repmgr.so, pgaudit.so), which Debian installs per
// major alongside the binaries. Used to tell "the data directory asks for a library that
// is genuinely not in this image" apart from "the library is there and simply unused"
// (#293): the former is a postmaster that will not start, and deserves a message naming
// the migration rather than PostgreSQL's bare `could not access file`.
func (c Config) PGLibdir() string { return "/usr/lib/postgresql/" + c.PGMajor + "/lib" }

// String renders the config with the repmgr password redacted, so logging the
// config (e.g. at startup) never leaks the secret. fmt uses this for %v/%s/%+v.
func (c Config) String() string {
	return fmt.Sprintf("Config{PodName:%s Namespace:%s LeaseName:%s "+
		"LeaseDuration:%s RenewDeadline:%s RetryPeriod:%s ReconcileInterval:%s "+
		"HeadlessService:%s NodeCount:%d MasterService:%s MarkerName:%s PodSelector:%q "+
		"RepmgrUser:%s RepmgrDB:%s RepmgrPassword:*** PGDATA:%s PGMajor:%s Mechanism:%s DCSBackend:%s "+
		"EtcdEndpoints:%v EtcdPrefix:%s EtcdTLS:%t PgHbaPeerCIDR:%s PgHbaRules:%d "+
		"Control:%t ControlAddr:%s ControlMTLS:%t ControlAllowedCNs:%d "+
		"ControlRestore:%t ControlRestoreAllowedCNs:%d ControlRestoreReadPodLogs:%t}",
		c.PodName, c.Namespace, c.LeaseName,
		c.LeaseDuration, c.RenewDeadline, c.RetryPeriod, c.ReconcileInterval,
		c.HeadlessService, c.NodeCount, c.MasterService, c.MarkerName, c.PodSelector,
		c.RepmgrUser, c.RepmgrDB, c.PGDATA, c.PGMajor, c.Mechanism, c.DCSBackend,
		c.EtcdEndpoints, c.EtcdPrefix, c.EtcdCertFile != "", c.PgHbaPeerCIDR, len(c.PgHbaRules),
		c.ControlEnabled, c.ControlAddr, c.ControlCertFile != "", len(c.ControlAllowedCNs),
		c.ControlRestoreEnabled, len(c.ControlRestoreAllowedCNs), c.ControlRestoreReadPodLogs)
}
