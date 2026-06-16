package k8s

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchemaVersion is the version of the data the agent writes to the DCS (the marker
// ConfigMap and the gossip pod-status). It is stamped on writes and checked on
// reads so a rolling agent upgrade -- which transiently runs mixed versions -- can
// detect an incompatible schema instead of silently misreading it (Part H4). v1
// fields (marker primary/timeline, gossip tl/lsn) are stable; readers tolerate a
// MISSING version (legacy data == v1) and ignore unknown fields, so the same minor
// stays forward/backward-compatible. A future breaking change bumps this and the
// older agent logs + degrades rather than corrupting state.
const SchemaVersion = 1

// PauseAnnotation, when set to "true" on the marker ConfigMap, puts the agent in
// maintenance mode (Part H1): it keeps renewing the Lease and serving but suspends
// automatic promote/demote/fence/self-health. An annotation (not a Data key) is
// used so it survives WriteMarker, which rewrites only the ConfigMap's Data. Toggle
// with `kubectl annotate configmap <fullname>-primary pg-ha/pause=true` (and
// `pg-ha/pause-` to resume).
const PauseAnnotation = "pg-ha/pause"

// SwitchoverTargetAnnotation, set to a pod name on the marker ConfigMap, requests
// a controlled handoff to that pod (Part H2). The serving primary steps down for
// it only once the target is a caught-up same-timeline standby, then clears the
// annotation (one-shot, so a later unrelated failover cannot re-trigger it). Set
// with `kubectl annotate configmap <fullname>-primary pg-ha/switchover-target=<pod>`.
const SwitchoverTargetAnnotation = "pg-ha/switchover-target"

// Marker is the durable highwater primary marker (<fullname>-primary ConfigMap):
// the highest-timeline primary ever recorded, so a node booting first under
// OrderedReady can tell it is stale (#125). Malformed is set when the marker
// exists but its timeline is missing or unparseable — callers must fail closed
// (#174), never treating it as "no constraint".
type Marker struct {
	Present    bool
	Malformed  bool
	Primary    string
	Timeline   uint32
	TimelineOK bool
	Paused     bool // maintenance mode: PauseAnnotation == "true" on the ConfigMap
	// SwitchoverTarget is the pod named by SwitchoverTargetAnnotation ("" if none).
	SwitchoverTarget string
	// SchemaVersion is the on-DCS data version (absent/0 == legacy v1). A reader
	// seeing a value above its own SchemaVersion is talking to a newer agent
	// mid-upgrade (Part H4).
	SchemaVersion int
}

// ReadMarker reads the marker ConfigMap. A missing marker is Present=false (not an
// error). A present marker with a missing/unparseable timeline is Malformed.
func (c *Client) ReadMarker(ctx context.Context, name string) (Marker, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Marker{Present: false}, nil
	}
	if err != nil {
		return Marker{}, fmt.Errorf("get marker %s: %w", name, err)
	}
	m := Marker{
		Present:          true,
		Primary:          cm.Data["primary"],
		Paused:           strings.EqualFold(strings.TrimSpace(cm.Annotations[PauseAnnotation]), "true"),
		SwitchoverTarget: strings.TrimSpace(cm.Annotations[SwitchoverTargetAnnotation]),
	}
	if v, perr := strconv.Atoi(cm.Data["schemaVersion"]); perr == nil {
		m.SchemaVersion = v
	} // absent/unparseable -> 0 == legacy v1 (a repmgrd-mode service-updater marker)
	tlStr, ok := cm.Data["timeline"]
	if !ok || tlStr == "" {
		m.Malformed = true
		return m, nil
	}
	v, perr := strconv.ParseUint(tlStr, 10, 32)
	if perr != nil {
		m.Malformed = true
		return m, nil
	}
	m.Timeline, m.TimelineOK = uint32(v), true
	return m, nil
}

// WriteMarker records primary + timeline (decimal) in the marker ConfigMap,
// creating it if absent. Callers advance it monotonically (write only when the
// timeline is at least the recorded highwater).
func (c *Client) WriteMarker(ctx context.Context, name, primary string, timeline uint32) error {
	data := map[string]string{
		"primary":       primary,
		"timeline":      strconv.FormatUint(uint64(timeline), 10),
		"schemaVersion": strconv.Itoa(SchemaVersion),
	}
	cms := c.cs.CoreV1().ConfigMaps(c.namespace)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, cerr := cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace},
			Data:       data,
		}, metav1.CreateOptions{})
		if cerr != nil {
			return fmt.Errorf("create marker %s: %w", name, cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get marker %s: %w", name, err)
	}
	// Merge our keys into the existing Data rather than replacing the whole map, so
	// any other keys on the marker ConfigMap (operator annotations-as-data, future
	// schema fields) survive a marker advance.
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for k, v := range data {
		cm.Data[k] = v
	}
	if _, uerr := cms.Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
		return fmt.Errorf("update marker %s: %w", name, uerr)
	}
	return nil
}

// ClearSwitchoverTarget removes the switchover-target annotation from the marker
// ConfigMap so a controlled switchover is one-shot -- a later, unrelated failover
// cannot re-trigger a handoff to the same pod. A missing marker or already-absent
// annotation is a no-op (nil).
func (c *Client) ClearSwitchoverTarget(ctx context.Context, name string) error {
	cms := c.cs.CoreV1().ConfigMaps(c.namespace)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get marker %s: %w", name, err)
	}
	if _, ok := cm.Annotations[SwitchoverTargetAnnotation]; !ok {
		return nil
	}
	delete(cm.Annotations, SwitchoverTargetAnnotation)
	if _, uerr := cms.Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
		return fmt.Errorf("clear switchover annotation on %s: %w", name, uerr)
	}
	return nil
}
