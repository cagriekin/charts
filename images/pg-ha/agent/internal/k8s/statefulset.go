package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StatefulSetReplicas returns spec.replicas of the named StatefulSet: the EXPECTED pod
// count, read live from the API (#373).
//
// This replaces REPMGR_NODE_COUNT as the agent's source for "how many nodes should there
// be". That env was rendered from postgresql.replicaCount into the pod template, so every
// scale of the database rolled every pod -- the primary included -- only to hand each
// agent a new integer. It was also stale on every pod that had not rolled yet, which is
// the trap orphanSlot and ListPodNames document. spec.replicas is what the StatefulSet
// controller is converging on, so a pod that is about to be created is already counted and
// a pod that is about to be trimmed is already excluded; the live pod list (ListPodNames)
// remains the authority on what EXISTS, and the two are used for different questions.
//
// A nil spec.replicas means the API default of one. The Get is scoped by the chart's RBAC
// to the one StatefulSet the agent belongs to.
func (c *Client) StatefulSetReplicas(ctx context.Context, name string) (int, error) {
	sts, err := c.cs.AppsV1().StatefulSets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get statefulset %s: %w", name, err)
	}
	if sts.Spec.Replicas == nil {
		return 1, nil
	}
	return int(*sts.Spec.Replicas), nil
}
