package kubecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig writes a minimal but valid kubeconfig and returns its path.
func writeKubeconfig(t *testing.T, dir, name, server, tlsServerName string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "apiVersion: v1\nkind: Config\ncurrent-context: c\nclusters:\n- name: cl\n  cluster:\n    server: " + server + "\n"
	if tlsServerName != "" {
		body += "    tls-server-name: " + tlsServerName + "\n"
	}
	body += "    insecure-skip-tls-verify: true\ncontexts:\n- name: c\n  context:\n    cluster: cl\n    user: u\nusers:\n- name: u\n  user:\n    token: t\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// The whole point of #317: the address comes from the kubeconfig, and the name the
// certificate is verified against can differ from it. Overriding
// KUBERNETES_SERVICE_HOST can express the first but never the second.
func TestRESTConfigHonoursKubeconfigServerAndTLSServerName(t *testing.T) {
	path := writeKubeconfig(t, t.TempDir(), "config", "https://apiserver-proxy.kube-system.svc:8443", "kubernetes.default.svc")
	t.Setenv("KUBECONFIG", path)

	rc, err := RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if rc.Host != "https://apiserver-proxy.kube-system.svc:8443" {
		t.Errorf("Host = %q, want the kubeconfig server", rc.Host)
	}
	if rc.TLSClientConfig.ServerName != "kubernetes.default.svc" {
		t.Errorf("ServerName = %q, want the kubeconfig tls-server-name", rc.TLSClientConfig.ServerName)
	}
	if rc.BearerToken != "t" {
		t.Errorf("BearerToken = %q, want the kubeconfig user token", rc.BearerToken)
	}
}

// KUBECONFIG may name several files; the earlier entry wins. Using ExplicitPath
// instead of the loading rules would treat the whole list as one filename.
func TestRESTConfigMergesTheKubeconfigListInPrecedenceOrder(t *testing.T) {
	dir := t.TempDir()
	first := writeKubeconfig(t, dir, "first", "https://first:8443", "")
	second := writeKubeconfig(t, dir, "second", "https://second:8443", "")
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)

	rc, err := RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if rc.Host != "https://first:8443" {
		t.Errorf("Host = %q, want the first file in KUBECONFIG to win", rc.Host)
	}
}

// A mounted-but-wrong path must fail loudly. Falling back to in-cluster here would
// reproduce the exact silent hang the escape hatch exists to escape.
func TestRESTConfigFailsWhenKUBECONFIGNamesNoReadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent", "kubeconfig")
	t.Setenv("KUBECONFIG", missing)

	_, err := RESTConfig()
	if err == nil {
		t.Fatal("RESTConfig succeeded with a missing KUBECONFIG; want an error")
	}
	// The message must name the file, not client-go's generic
	// "no configuration has been provided, try setting KUBERNETES_MASTER".
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the missing kubeconfig %q", err, missing)
	}
}

func TestRESTConfigFailsOnAMalformedKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("{{ not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)

	if _, err := RESTConfig(); err == nil {
		t.Fatal("RESTConfig succeeded on a malformed kubeconfig; want an error")
	}
}

// A valid kubeconfig with no usable context is still a failure, not a fallback.
func TestRESTConfigFailsOnAnEmptyKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)

	if _, err := RESTConfig(); err == nil {
		t.Fatal("RESTConfig succeeded on an empty kubeconfig; want an error")
	}
}

// With KUBECONFIG unset the behaviour is the pre-#317 in-cluster path verbatim,
// including its failure mode outside a cluster. This is the default deployment, so
// it is the case that must not move.
func TestRESTConfigWithoutKUBECONFIGUsesTheInClusterPath(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	// Deliberately not stubbing the ServiceAccount: outside a cluster the in-cluster
	// loader must be the thing that fails, which is what the message proves.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	_, err := RESTConfig()
	if err == nil {
		t.Fatal("RESTConfig succeeded outside a cluster with no KUBECONFIG; want the in-cluster error")
	}
	if !strings.Contains(err.Error(), "in-cluster config") {
		t.Errorf("error %q is not the in-cluster path's error", err)
	}
}

// An empty KUBECONFIG (set-but-blank, e.g. `env KUBECONFIG=`) must not be treated as
// a kubeconfig request; a blank value is how a values overlay ends up nulling it out.
func TestRESTConfigTreatsAnEmptyKUBECONFIGAsUnset(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	if _, err := RESTConfig(); err == nil || !strings.Contains(err.Error(), "in-cluster config") {
		t.Fatalf("err = %v, want the in-cluster path's error", err)
	}
}

// #317's log line: the two routes must be distinguishable in the boot log, because
// the failure they produce (a dial timeout) reads identically.
func TestSourceNamesTheRoute(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	if got := Source(); got != "in-cluster" {
		t.Errorf("Source() = %q, want in-cluster", got)
	}
	t.Setenv("KUBECONFIG", "/etc/apiserver-proxy/kubeconfig")
	if got := Source(); got != "kubeconfig /etc/apiserver-proxy/kubeconfig" {
		t.Errorf("Source() = %q, want the kubeconfig path", got)
	}
}
