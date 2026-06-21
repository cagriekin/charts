// Package pgconf owns PostgreSQL file configuration in agent mode (replacing the
// chart's postStart hook + setup-config init container): it assembles a hardened,
// SCRAM-only pg_hba.conf and manages the conf.d include line. The SQL-side
// md5->scram re-hash of managed users and the primary-only additionalCommands run
// from the supervisor (they need a live DB) and are layered there.
package pgconf

import (
	"fmt"
	"os"
	"strings"
)

// PgHbaOptions describes the agent-assembled pg_hba.conf.
type PgHbaOptions struct {
	ReplicationUser string   // repmgr replication user
	PeerCIDR        string   // the trusted pod network (e.g. 10.0.0.0/8)
	ExtraRules      []string // user-supplied postgresql.pgHba, placed ABOVE the network catch-alls (#144)

	// Client TLS (issue #110), optional. RequireSSL makes the peer-CIDR client rule
	// `hostssl` (reject non-TLS clients). ClientCertAuth additionally requires a
	// client cert (clientcert=verify-ca) for app/other users, while the internal
	// service users below are EXEMPTED (they authenticate by password over TLS and
	// hold no client cert): the agent prober / repmgr CLI (ReplicationUser), pgpool
	// health + service-updater (PostgresUser), and the exporter (MonitoringUser).
	// Loopback and the replication rule are never converted -- replication stays
	// plaintext on the pod network (#110 non-goal).
	RequireSSL     bool
	ClientCertAuth bool
	PostgresUser   string // superuser, exempt from clientcert
	MonitoringUser string // monitoring user, exempt from clientcert; "" to skip
}

// AssemblePgHba returns a hardened pg_hba.conf: loopback + the pod CIDR over
// SCRAM only. It deliberately omits the legacy `0.0.0.0/0 md5` catch-alls
// (security review C1) — agent mode is secure by construction; managed users are
// re-hashed to SCRAM by the supervisor. User rules are inserted above the network
// catch-alls so they take precedence (#144 ordering).
func AssemblePgHba(o PgHbaOptions) string {
	var b strings.Builder
	b.WriteString("# Managed by pg-ha-agent (agent mode); edits are overwritten.\n")
	b.WriteString("local       all           all                              trust\n")
	b.WriteString("host        all           all        127.0.0.1/32          scram-sha-256\n")
	b.WriteString("host        all           all        ::1/128               scram-sha-256\n")
	if len(o.ExtraRules) > 0 {
		b.WriteString("# user-supplied postgresql.pgHba (above the network catch-alls)\n")
		for _, r := range o.ExtraRules {
			if line := strings.TrimRight(r, "\n"); line != "" {
				b.WriteString(line + "\n")
			}
		}
	}
	// Replication stays plaintext (repmgr/agent conninfo carries no sslmode) -- never
	// hostssl, even under require/mTLS (#110 non-goal).
	fmt.Fprintf(&b, "host        replication   %s        %s        scram-sha-256\n", o.ReplicationUser, o.PeerCIDR)

	switch {
	case o.ClientCertAuth:
		// mTLS: hostssl, with per-user exemptions for the internal service users
		// (no clientcert) above the app catch-all (clientcert=verify-ca) (#110).
		fmt.Fprintf(&b, "hostssl     all           %s        %s        scram-sha-256\n", o.ReplicationUser, o.PeerCIDR)
		if o.PostgresUser != "" {
			fmt.Fprintf(&b, "hostssl     all           %s        %s        scram-sha-256\n", o.PostgresUser, o.PeerCIDR)
		}
		if o.MonitoringUser != "" {
			fmt.Fprintf(&b, "hostssl     all           %s        %s        scram-sha-256\n", o.MonitoringUser, o.PeerCIDR)
		}
		fmt.Fprintf(&b, "hostssl     all           all        %s        scram-sha-256 clientcert=verify-ca\n", o.PeerCIDR)
	case o.RequireSSL:
		fmt.Fprintf(&b, "hostssl     all           all        %s        scram-sha-256\n", o.PeerCIDR)
	default:
		fmt.Fprintf(&b, "host        all           all        %s        scram-sha-256\n", o.PeerCIDR)
	}
	return b.String()
}

// WritePgHba writes pg_hba.conf at path (0600).
func WritePgHba(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write pg_hba.conf: %w", err)
	}
	return nil
}

// EnsureConfdInclude idempotently adds or removes the managed `include_dir` line
// for includeDir in postgresql.conf (ports the setup-config init container). It
// first strips any existing managed line, then appends it when enabled, so
// repeated calls converge and toggling off cleans up stale state.
func EnsureConfdInclude(confPath, includeDir string, enabled bool) error {
	managed := fmt.Sprintf("include_dir = '%s'", includeDir)
	data, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", confPath, err)
	}
	kept := make([]string, 0)
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) == managed {
			continue
		}
		kept = append(kept, ln)
	}
	content := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if enabled {
		content += "\n" + managed
	}
	content += "\n"
	if err := os.WriteFile(confPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", confPath, err)
	}
	return nil
}
