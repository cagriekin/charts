// Package podname parses StatefulSet pod names. It exists so the `<base>-<ordinal>`
// convention has exactly ONE parser (#298 review): there were three -- cmd/agent's
// podOrdinal returning (int, bool), reconcile's returning a bare int sentinel, and an
// inline strings.LastIndex in cmd/agent's baseName -- and they did not agree at the edges.
// cmd/agent accepted "-0" (LastIndex 0, no base name) where reconcile rejected it, so the
// same pod name could be an ordinal to the slot-reclaim code and unparseable to the
// promote-distance code. Slot reclaim DROPS things; a parser disagreement there is not
// stylistic.
//
// This package holds no Kubernetes dependency on purpose: internal/reconcile is pure
// policy and must stay importable without a cluster.
package podname

import (
	"strconv"
	"strings"
)

// Ordinal splits a StatefulSet pod name into its ordinal. It requires a non-empty base
// name before the separator, so "-0" and "0" are both rejected: a StatefulSet pod is
// always `<statefulset-name>-<n>`, and accepting a bare ordinal would let an arbitrary
// numeric string be read as this cluster's pod.
func Ordinal(pod string) (int, bool) {
	i := strings.LastIndex(pod, "-")
	if i <= 0 || i == len(pod)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(pod[i+1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// OrdinalOr returns Ordinal's value, or def when the name does not parse. For the callers
// that want a sentinel to do arithmetic on rather than a boolean to branch on.
func OrdinalOr(pod string, def int) int {
	if n, ok := Ordinal(pod); ok {
		return n
	}
	return def
}

// Base strips the trailing -<ordinal>, returning the StatefulSet name. Returns pod
// unchanged when there is nothing to strip, so a caller building a peer FQDN from it
// degrades to "use the name as given" rather than to an empty host.
func Base(pod string) string {
	if _, ok := Ordinal(pod); !ok {
		return pod
	}
	return pod[:strings.LastIndex(pod, "-")]
}
