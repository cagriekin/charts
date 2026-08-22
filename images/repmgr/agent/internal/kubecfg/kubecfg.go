// Package kubecfg resolves how the agent reaches the Kubernetes apiserver.
//
// It is its own package because two independent clients need the same answer and
// neither should own it: the mutation client (internal/k8s, which patches the write
// Service and the primary marker) and the Lease-backed DCS (internal/dcs).
package kubecfg

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RESTConfig resolves the apiserver connection for this process.
//
// KUBECONFIG wins when set; with it unset the in-cluster ServiceAccount plus
// KUBERNETES_SERVICE_HOST/PORT are used, which is the default deployment and is
// exactly the previous rest.InClusterConfig() behaviour (#317).
//
// The kubeconfig escape hatch exists for clusters whose egress policy denies pod
// traffic to the apiserver outright, where the agent otherwise never reads its
// primary marker and the cluster never elects a leader. Two properties can make
// that unfixable from the policy side (reported on Cilium 1.20): deny wins within
// a tier, so no allow rule re-opens the apiserver for one namespace; and reserved
// identities are compound -- `reserved:host` and `reserved:kube-apiserver` sit on
// the same identity -- so any topology reaching the apiserver via a real node IP
// cannot admit apiserver traffic for one workload without admitting node traffic
// for it. What remains is an in-cluster TCP proxy, and reaching one needs a
// different *address* while still verifying the apiserver's *certificate*, whose
// SANs cover kubernetes.default.svc and the apiserver IPs but not the proxy
// Service. Only a kubeconfig expresses that pair (`server:` + `tls-server-name:`);
// overriding KUBERNETES_SERVICE_HOST retargets the dial but leaves no way to set
// ServerName, so it breaks verification instead of fixing routing.
//
// A KUBECONFIG that is set but unusable is a hard error, deliberately. client-go's
// DeferredLoadingClientConfig would silently fall back to in-cluster here, which
// would reproduce the very hang this hatch exists to escape -- and do it with a
// kubeconfig mounted and apparently in effect, which is strictly harder to
// diagnose than a startup failure naming the file.
func RESTConfig() (*rest.Config, error) {
	path := os.Getenv(clientcmd.RecommendedConfigPathEnvVar)
	if path == "" {
		rc, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return rc, nil
	}
	return kubeconfigREST(path)
}

// Source names the route RESTConfig will take. It exists so boot can log which
// apiserver address this agent is actually using: the failure this fixes looks
// identical in the logs either way -- a dial timeout to an address nothing prints.
func Source() string {
	if path := os.Getenv(clientcmd.RecommendedConfigPathEnvVar); path != "" {
		return "kubeconfig " + path
	}
	return "in-cluster"
}

// kubeconfigREST loads the merged kubeconfig named by KUBECONFIG and converts its
// current context to a rest.Config, with no in-cluster fallback.
//
// The loading rules are built by NewDefaultClientConfigLoadingRules rather than by
// setting ExplicitPath, because KUBECONFIG may name several files separated by the
// OS list separator and only the rules know how to dedupe and merge that
// precedence chain. NewDefaultClientConfig (a DirectClientConfig) is then used
// instead of the deferred loader precisely so an empty or malformed result surfaces
// as an error rather than as a silent return to the in-cluster route.
func kubeconfigREST(path string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	// Load() skips files that do not exist and reports "all of them were missing"
	// only through the Warner callback, returning an empty Config whose eventual
	// error is the generic ErrEmptyConfig ("try setting KUBERNETES_MASTER"). Capturing
	// the warning instead lets the failure name the file the operator mounted.
	var allMissing error
	rules.Warner = func(err error) { allMissing = err }
	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig (KUBECONFIG=%s): %w", path, err)
	}
	if allMissing != nil {
		return nil, fmt.Errorf("kubeconfig (KUBECONFIG=%s): %w", path, allMissing)
	}
	rc, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig (KUBECONFIG=%s): %w", path, err)
	}
	return rc, nil
}
