package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const podNameSelectorKey = "statefulset.kubernetes.io/pod-name"

// PatchWriteSelector points the write Service at podName via its
// statefulset.kubernetes.io/pod-name selector. It is idempotent: if the selector
// already targets podName it returns changed=false without an API write.
func (c *Client) PatchWriteSelector(ctx context.Context, service, podName string) (changed bool, err error) {
	svc, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get service %s: %w", service, err)
	}
	if svc.Spec.Selector[podNameSelectorKey] == podName {
		return false, nil
	}
	patch := fmt.Sprintf(`{"spec":{"selector":{%q:%q}}}`, podNameSelectorKey, podName)
	if _, err := c.cs.CoreV1().Services(c.namespace).Patch(ctx, service, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("patch service %s selector: %w", service, err)
	}
	return true, nil
}

// ReconcilePodLabels sets pg-role on the pods named in desired (podName -> role,
// e.g. "primary"/"standby"/"orphan"). Pods absent from desired (or with an empty
// role) are left untouched — the #140 contract: an unreachable node we cannot
// classify keeps its current label rather than being churned. Each pod is patched
// only when its current pg-role differs.
func (c *Client) ReconcilePodLabels(ctx context.Context, labelSelector string, desired map[string]string) error {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		want, ok := desired[p.Name]
		if !ok || want == "" {
			continue
		}
		if p.Labels["pg-role"] == want {
			continue
		}
		patch := fmt.Sprintf(`{"metadata":{"labels":{"pg-role":%q}}}`, want)
		if _, err := c.cs.CoreV1().Pods(c.namespace).Patch(ctx, p.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("label pod %s: %w", p.Name, err)
		}
	}
	return nil
}
