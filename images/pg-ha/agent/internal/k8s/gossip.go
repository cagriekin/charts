package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// StatusAnnotation is the pod annotation the agent gossips its WAL position
// through. Each node writes only its OWN pod (single writer per object, no
// contention) and the lease holder reads peers' annotations, so it can rank a
// stopped/unreachable peer at cold-boot election time -- closing the
// same-timeline most-advanced gap the timeline-granular marker cannot.
const StatusAnnotation = "pg-ha/status"

// NodeStatus is one node's self-reported WAL position. Primitive fields keep this
// package free of the pg types (the agent converts). UpdatedAtUnix lets a reader
// ignore stale gossip left by a wedged/dead agent (clocks assumed NTP-synced, as
// the soft-fence already requires).
type NodeStatus struct {
	Timeline      uint32 `json:"tl"`
	TimelineOK    bool   `json:"tlOK"`
	LSNHi         uint64 `json:"lsnHi"`
	LSNLo         uint64 `json:"lsnLo"`
	LSNOK         bool   `json:"lsnOK"`
	UpdatedAtUnix int64  `json:"ts"`
	// RestoredAt is the FinishedAt of the last SUCCESSFUL pgBackRest restore on this node's
	// data volume, or "" when the volume carries no such record (#288).
	//
	// It is the cross-node half of restore authority. A PITR restore deliberately rewinds the
	// cluster, so the restored volume is BEHIND its un-restored peers in LSN terms -- and the
	// LSN-based election, doing its job, would then let a stale peer win and discard the
	// restored history. The restore record is durable on the volume that was restored and is
	// dropped whenever that volume is re-cloned, which makes it exactly the right provenance
	// to compare. Empty for every node that has never been restored, which is the norm.
	RestoredAt string `json:"rst,omitempty"`
	// SchemaVersion is the on-DCS data version (omitted/0 == legacy v1). Stamped on
	// publish so a peer reading this can detect a newer agent mid-upgrade (Part H4).
	SchemaVersion int `json:"v,omitempty"`
}

// PublishStatus merge-patches this node's status onto its own pod annotation.
func (c *Client) PublishStatus(ctx context.Context, podName string, st NodeStatus) error {
	if st.SchemaVersion == 0 {
		st.SchemaVersion = SchemaVersion
	}
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	// The annotation value is itself a JSON string, so encode it again to escape
	// the embedded quotes inside the merge patch.
	val, err := json.Marshal(string(b))
	if err != nil {
		return fmt.Errorf("encode status value: %w", err)
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%s}}}`, StatusAnnotation, val)
	if _, err := c.cs.CoreV1().Pods(c.namespace).Patch(ctx, podName, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch pod %s status: %w", podName, err)
	}
	return nil
}

// ListPodNames returns the names of every pod matching labelSelector, including self and
// including pods that have published no gossip status yet.
//
// This is deliberately NOT ReadPeerStatuses: that one filters to pods carrying a parseable
// status annotation, which is the wrong question for "does this pod exist". A pod that is
// mid-restart, still starting, or has never published gossip is absent from that map but
// very much present in the cluster -- and treating it as gone is how a replication slot
// that is still needed gets reclaimed (#289). The live pod list is also the only source
// that stays correct during a scale-up rollout, when REPMGR_NODE_COUNT is baked into each
// pod's env at render time and is therefore STALE on every pod that has not rolled yet.
func (c *Client) ListPodNames(ctx context.Context, labelSelector string) ([]string, error) {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		names = append(names, pods.Items[i].Name)
	}
	return names, nil
}

// ReadPeerStatuses lists pods matching labelSelector and returns each peer's
// gossiped status keyed by pod name, excluding self. Pods missing the annotation
// or carrying an unparseable value are skipped (treated as no gossip). Freshness
// is the caller's concern (it has the clock + staleness policy).
func (c *Client) ReadPeerStatuses(ctx context.Context, labelSelector, self string) (map[string]NodeStatus, error) {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	out := make(map[string]NodeStatus)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Name == self {
			continue
		}
		raw, ok := p.Annotations[StatusAnnotation]
		if !ok || raw == "" {
			continue
		}
		var st NodeStatus
		if json.Unmarshal([]byte(raw), &st) != nil {
			continue
		}
		out[p.Name] = st
	}
	return out, nil
}
