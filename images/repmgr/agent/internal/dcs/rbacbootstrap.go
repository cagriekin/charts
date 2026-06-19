package dcs

import (
	"context"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// RBACTenant is one per-tenant grant: a user (matched by its client-cert Common
// Name under --client-cert-auth) authorized readwrite on exactly its key prefix.
type RBACTenant struct {
	CommonName string `json:"commonName"`
	Prefix     string `json:"prefix"`
}

// RBACBootstrap idempotently provisions etcd RBAC for the shared-etcd topology and
// enables auth. It reuses the agent's mTLS client (no etcdctl, no shell -- the
// bundled etcd image is distroless), so the bootstrap runs in the repmgr image.
//
// Order matters: the root user+role must exist before AuthEnable, and every tenant
// user must exist before auth flips on (so already-connected agents keep
// authenticating by CN). Every step is existence-checked, so re-running on each
// helm upgrade reconciles the tenant list without erroring on what already exists.
func RBACBootstrap(ctx context.Context, endpoints []string, certFile, keyFile, caFile, rootCN string, tenants []RBACTenant) error {
	if len(endpoints) == 0 {
		return fmt.Errorf("rbac-bootstrap: no ETCD_ENDPOINTS")
	}
	if rootCN == "" {
		return fmt.Errorf("rbac-bootstrap: no ETCD_RBAC_ROOT_CN")
	}
	tlsCfg, err := EtcdConfig{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}.tlsConfig()
	if err != nil {
		return err
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second, TLS: tlsCfg})
	if err != nil {
		return fmt.Errorf("rbac-bootstrap: etcd client: %w", err)
	}
	defer cli.Close()

	if err := waitHealthy(ctx, cli, endpoints[0]); err != nil {
		return err
	}

	// root user + role first (AuthEnable requires a root user holding the root role).
	if err := ensureUser(ctx, cli, rootCN); err != nil {
		return err
	}
	if err := ensureRole(ctx, cli, "root"); err != nil {
		return err
	}
	if err := ensureUserRole(ctx, cli, rootCN, "root"); err != nil {
		return err
	}

	for _, t := range tenants {
		if t.CommonName == "" || t.Prefix == "" {
			return fmt.Errorf("rbac-bootstrap: tenant needs commonName and prefix, got %+v", t)
		}
		if err := ensureUser(ctx, cli, t.CommonName); err != nil {
			return err
		}
		if err := ensureRole(ctx, cli, t.CommonName); err != nil {
			return err
		}
		if err := ensurePrefixPerm(ctx, cli, t.CommonName, t.Prefix); err != nil {
			return err
		}
		if err := ensureUserRole(ctx, cli, t.CommonName, t.CommonName); err != nil {
			return err
		}
	}

	st, err := cli.AuthStatus(ctx)
	if err != nil {
		return fmt.Errorf("rbac-bootstrap: auth status: %w", err)
	}
	if !st.Enabled {
		// Refuse to enable auth with no tenants: every agent authenticates by its
		// client-cert CN, so enabling auth with only the root user would deny every
		// agent's next request and strand leadership cluster-wide. (The chart also
		// guards this at render; this is defense-in-depth for a direct invocation.)
		if len(tenants) == 0 {
			return fmt.Errorf("rbac-bootstrap: refusing to enable auth with no tenants (would lock out every CN-authenticated agent)")
		}
		if _, err := cli.AuthEnable(ctx); err != nil {
			return fmt.Errorf("rbac-bootstrap: auth enable: %w", err)
		}
	}
	return nil
}

// waitHealthy blocks until a Status call succeeds (etcd may still be forming when
// the post-install hook runs), up to ~90s.
func waitHealthy(ctx context.Context, cli *clientv3.Client, endpoint string) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := cli.Status(sctx, endpoint)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rbac-bootstrap: etcd not reachable within deadline: %w", err)
		}
		time.Sleep(3 * time.Second)
	}
}

func ensureUser(ctx context.Context, cli *clientv3.Client, name string) error {
	users, err := cli.UserList(ctx)
	if err != nil {
		return fmt.Errorf("rbac-bootstrap: user list: %w", err)
	}
	if contains(users.Users, name) {
		return nil
	}
	if _, err := cli.UserAddWithOptions(ctx, name, "", &clientv3.UserAddOptions{NoPassword: true}); err != nil {
		return fmt.Errorf("rbac-bootstrap: user add %q: %w", name, err)
	}
	return nil
}

func ensureRole(ctx context.Context, cli *clientv3.Client, name string) error {
	roles, err := cli.RoleList(ctx)
	if err != nil {
		return fmt.Errorf("rbac-bootstrap: role list: %w", err)
	}
	if contains(roles.Roles, name) {
		return nil
	}
	if _, err := cli.RoleAdd(ctx, name); err != nil {
		return fmt.Errorf("rbac-bootstrap: role add %q: %w", name, err)
	}
	return nil
}

func ensureUserRole(ctx context.Context, cli *clientv3.Client, user, role string) error {
	u, err := cli.UserGet(ctx, user)
	if err != nil {
		return fmt.Errorf("rbac-bootstrap: user get %q: %w", user, err)
	}
	if contains(u.Roles, role) {
		return nil
	}
	if _, err := cli.UserGrantRole(ctx, user, role); err != nil {
		return fmt.Errorf("rbac-bootstrap: grant role %q to %q: %w", role, user, err)
	}
	return nil
}

// ensurePrefixPerm grants readwrite on [prefix, prefixEnd) to the role if not
// already present (etcd dedups identical grants, but the check keeps it explicit).
func ensurePrefixPerm(ctx context.Context, cli *clientv3.Client, role, prefix string) error {
	end := clientv3.GetPrefixRangeEnd(prefix)
	r, err := cli.RoleGet(ctx, role)
	if err != nil {
		return fmt.Errorf("rbac-bootstrap: role get %q: %w", role, err)
	}
	for _, p := range r.Perm {
		if string(p.Key) == prefix && string(p.RangeEnd) == end && p.PermType == clientv3.PermReadWrite {
			return nil
		}
	}
	if _, err := cli.RoleGrantPermission(ctx, role, prefix, end, clientv3.PermissionType(clientv3.PermReadWrite)); err != nil {
		return fmt.Errorf("rbac-bootstrap: grant %q readwrite on %q: %w", role, prefix, err)
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
