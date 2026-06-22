// Package pgconf owns PostgreSQL file configuration in agent mode (replacing the
// chart's postStart hook + setup-config init container): it assembles pg_hba.conf
// (no legacy 0.0.0.0/0 catch-all, plus an optional md5-first compat layer for
// clusters carrying md5-stored passwords -- #199) and manages the conf.d include
// line. The agent is the single author of pg_hba.conf in agent mode; the SQL-side
// md5->scram re-hash of managed users runs from the agent on promotion/boot-primary.
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

	// MD5Fallback emits an `md5` line immediately above each chart-generated
	// `scram-sha-256` rule (loopback, replication, the peer-CIDR / hostssl / per-user
	// rules) -- never above the mTLS clientcert catch-all, and never for ExtraRules.
	// `md5` auth transparently authenticates both md5-stored and SCRAM-stored
	// passwords, so this is a compatibility layer (not a downgrade) for clusters
	// migrated from md5 hashes, on the trusted pod CIDR (#199). It is the single-author
	// replacement for the chart's former postStart md5-fallback awk, so every node
	// (primary and standby) ends up byte-identical.
	MD5Fallback bool

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

// AssemblePgHba returns the agent-authored pg_hba.conf: loopback + the pod CIDR over
// SCRAM, with no legacy `0.0.0.0/0 md5` catch-all (security review C1 -- the external
// superuser-exposure risk). With MD5Fallback set, an `md5` line is layered above each
// generated scram rule for clusters carrying md5-stored passwords (#199). User rules
// are inserted above the network catch-alls so they take precedence (#144 ordering).
func AssemblePgHba(o PgHbaOptions) string {
	var b strings.Builder
	b.WriteString("# Managed by pg-ha-agent (agent mode); edits are overwritten.\n")
	b.WriteString("local       all           all                              trust\n")
	emitRule(&b, "host        all           all        127.0.0.1/32          scram-sha-256", o.MD5Fallback)
	emitRule(&b, "host        all           all        ::1/128               scram-sha-256", o.MD5Fallback)
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
	emitRule(&b, fmt.Sprintf("host        replication   %s        %s        scram-sha-256", o.ReplicationUser, o.PeerCIDR), o.MD5Fallback)

	switch {
	case o.ClientCertAuth:
		// mTLS: hostssl, with per-user exemptions for the internal service users
		// (no clientcert) above the app catch-all (clientcert=verify-ca) (#110).
		emitRule(&b, fmt.Sprintf("hostssl     all           %s        %s        scram-sha-256", o.ReplicationUser, o.PeerCIDR), o.MD5Fallback)
		if o.PostgresUser != "" {
			emitRule(&b, fmt.Sprintf("hostssl     all           %s        %s        scram-sha-256", o.PostgresUser, o.PeerCIDR), o.MD5Fallback)
		}
		if o.MonitoringUser != "" {
			emitRule(&b, fmt.Sprintf("hostssl     all           %s        %s        scram-sha-256", o.MonitoringUser, o.PeerCIDR), o.MD5Fallback)
		}
		// The app catch-all requires a client cert; it is NEVER md5-fallback'd -- an md5
		// line above it would let app users authenticate by password and skip the cert (#199).
		fmt.Fprintf(&b, "hostssl     all           all        %s        scram-sha-256 clientcert=verify-ca\n", o.PeerCIDR)
	case o.RequireSSL:
		emitRule(&b, fmt.Sprintf("hostssl     all           all        %s        scram-sha-256", o.PeerCIDR), o.MD5Fallback)
	default:
		emitRule(&b, fmt.Sprintf("host        all           all        %s        scram-sha-256", o.PeerCIDR), o.MD5Fallback)
	}
	return b.String()
}

// emitRule writes a pg_hba rule line. With md5Fallback it first writes the same rule
// with the trailing `scram-sha-256` method replaced by `md5` (md5-first compat, #199):
// the md5 line matches first and authenticates both md5- and SCRAM-stored passwords.
// Callers must NOT pass the mTLS clientcert catch-all here (it must require a cert).
func emitRule(b *strings.Builder, line string, md5Fallback bool) {
	if md5Fallback {
		b.WriteString(strings.TrimSuffix(line, "scram-sha-256") + "md5\n")
	}
	b.WriteString(line + "\n")
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
