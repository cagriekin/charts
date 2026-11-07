package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// In-place pod resize (#386). The pod condition types and reasons below are the
// Kubernetes 1.33+ resize status (KEP-1287); they are spelled out rather than taken from
// k8s.io/api because the vendored v0.31 predates the constants, and bumping client-go for
// four strings would move every other API this agent calls.
const (
	condResizePending    = "PodResizePending"
	condResizeInProgress = "PodResizeInProgress"
	// ResizeDeferred: the size fits the node in principle but not right now (other pods
	// hold the room); the kubelet retries on its own.
	ResizeDeferred = "Deferred"
	// ResizeInfeasible: the size can never fit this node (above its allocatable).
	ResizeInfeasible = "Infeasible"
)

// ResizeRefusedInfeasible reports whether err is the apiserver refusing a resize because the
// pod's node cannot fit it at all. From Kubernetes 1.35 the apiserver checks node allocatable
// at admission and answers 403 "node didn't have enough allocatable resources" -- the request
// never reaches the kubelet, so no PodResizePending=Infeasible condition is ever set. It is the
// same outcome and is reported the same way (#386, found by the KinD suite).
func ResizeRefusedInfeasible(err error) bool {
	return err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "enough allocatable")
}

// ResizeRefusedTransient reports whether a failed resize patch is worth retrying soon (#386):
// a timeout, throttle, conflict or server-side error says nothing about the size. A refusal
// the apiserver will repeat (forbidden -- quota, allocatable, RBAC --, invalid, not found) is
// not, and is retried only on the slower recheck.
func ResizeRefusedTransient(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case apierrors.IsForbidden(err), apierrors.IsInvalid(err), apierrors.IsBadRequest(err),
		apierrors.IsNotFound(err), apierrors.IsMethodNotSupported(err), apierrors.IsUnauthorized(err):
		return false
	}
	return true
}

// resizeResources are the resources the agent manages on a resize. Anything else in a
// container's requests/limits (ephemeral-storage, hugepages, extended resources) is left
// exactly as the StatefulSet template rendered it.
var resizeResources = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

// PodResize is one read of this pod's resize state against a wanted size (#386).
type PodResize struct {
	// SpecMatches: every named container's spec requests/limits equal the wanted size.
	SpecMatches bool
	// Actuated: SpecMatches, the kubelet reports no resize pending or in progress, and every
	// named container's status.resources equals the wanted size -- the size is in effect.
	Actuated bool
	// Pending is the PodResizePending reason (ResizeDeferred / ResizeInfeasible), "" if none.
	Pending string
	// InProgress: the kubelet has accepted the resize and is applying it.
	InProgress bool
	// Message is the kubelet's message on whichever resize condition is set, for the log.
	Message string
	// Containers is the pod's spec container order, which ResizePod's patch must follow;
	// carried from this read so a resize costs one GET, not two.
	Containers []string
}

// PodResizeStatus reads pod and compares it with want, keyed by container name (#386).
func (c *Client) PodResizeStatus(ctx context.Context, pod string, want map[string]corev1.ResourceRequirements) (PodResize, error) {
	p, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return PodResize{}, fmt.Errorf("get pod %s: %w", pod, err)
	}
	return resizeStateOf(p, want)
}

func resizeStateOf(p *corev1.Pod, want map[string]corev1.ResourceRequirements) (PodResize, error) {
	var st PodResize
	for _, cond := range p.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch string(cond.Type) {
		case condResizePending:
			st.Pending = cond.Reason
			st.Message = cond.Message
		case condResizeInProgress:
			st.InProgress = true
			if st.Message == "" {
				st.Message = cond.Message
			}
		}
	}
	spec := map[string]corev1.ResourceRequirements{}
	for _, ctr := range p.Spec.Containers {
		spec[ctr.Name] = ctr.Resources
		st.Containers = append(st.Containers, ctr.Name)
	}
	status := map[string]*corev1.ResourceRequirements{}
	for _, cs := range p.Status.ContainerStatuses {
		status[cs.Name] = cs.Resources
	}
	st.SpecMatches, st.Actuated = true, true
	for name, w := range want {
		s, ok := spec[name]
		if !ok {
			// The chart and the env disagree about the pod's containers: that is a
			// misconfiguration no resize can fix, not a state to wait out.
			return PodResize{}, fmt.Errorf("pod %s has no container %q to resize", p.Name, name)
		}
		if !sameSize(s, w) {
			st.SpecMatches = false
		}
		if a := status[name]; a == nil || !sameSize(*a, w) {
			st.Actuated = false
		}
	}
	if !st.SpecMatches || st.Pending != "" || st.InProgress {
		st.Actuated = false
	}
	return st, nil
}

// sameSize compares the cpu/memory requests and limits by value: "1" and "1000m" are the
// same size, and the apiserver normalizes what it stores, so a string compare would see a
// difference on every read and re-patch forever.
func sameSize(a, b corev1.ResourceRequirements) bool {
	for _, r := range resizeResources {
		if !sameQty(a.Requests, b.Requests, r) || !sameQty(a.Limits, b.Limits, r) {
			return false
		}
	}
	return true
}

func sameQty(a, b corev1.ResourceList, r corev1.ResourceName) bool {
	qa, oka := a[r]
	qb, okb := b[r]
	if oka != okb {
		return false
	}
	return !oka || qa.Cmp(qb) == 0
}

// ResizePod sets the cpu/memory of the named containers of pod through the pods/resize
// subresource (#386): one strategic-merge patch for all of them, so postgresql and
// pgbackrest move to the same role's size together. The kubelet applies it without a
// restart (the chart sets resizePolicy NotRequired) or reports it Deferred/Infeasible.
//
// The patch lists the containers in the pod's OWN order. A strategic merge reorders the list
// to the patch's order, and the apiserver refuses any resize that renames or reorders
// containers -- a name-sorted patch against a pod whose spec runs postgresql before
// pgbackrest was rejected on every attempt (found by the KinD suite).
//
// order is the pod's spec container order, as PodResizeStatus read it (PodResize.Containers).
func (c *Client) ResizePod(ctx context.Context, pod string, want map[string]corev1.ResourceRequirements, order []string) error {
	body, err := resizePatch(want, order)
	if err != nil {
		return err
	}
	if _, err := c.cs.CoreV1().Pods(c.namespace).Patch(ctx, pod, types.StrategicMergePatchType, body, metav1.PatchOptions{}, "resize"); err != nil {
		return fmt.Errorf("resize pod %s: %w", pod, err)
	}
	return nil
}

// resizePatch builds the resize body, its containers in order (the pod's spec order). A
// wanted container missing from order is an error: the patch would otherwise append it and
// the apiserver would refuse the rename.
func resizePatch(want map[string]corev1.ResourceRequirements, order []string) ([]byte, error) {
	type res struct {
		Requests corev1.ResourceList `json:"requests"`
		Limits   corev1.ResourceList `json:"limits"`
	}
	type ctr struct {
		Name      string `json:"name"`
		Resources res    `json:"resources"`
	}
	var ctrs []ctr
	for _, n := range order {
		w, ok := want[n]
		if !ok {
			continue
		}
		r := res{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
		for _, k := range resizeResources {
			if q, ok := w.Requests[k]; ok {
				r.Requests[k] = q
			}
			if q, ok := w.Limits[k]; ok {
				r.Limits[k] = q
			}
		}
		ctrs = append(ctrs, ctr{Name: n, Resources: r})
	}
	if len(ctrs) != len(want) {
		return nil, fmt.Errorf("resize names a container the pod does not have (want %d, pod has %v)", len(want), order)
	}
	return json.Marshal(map[string]any{"spec": map[string]any{"containers": ctrs}})
}
