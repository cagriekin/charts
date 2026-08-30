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

// PausedByAnnotation and SwitchoverRequestedByAnnotation record WHO asked for the
// current pause / switchover when it was requested through the control API (#276).
// They are provenance only -- no agent logic reads them -- but they survive a pod
// restart and show up in `kubectl describe`, which the agent's own audit log (pod
// stdout) does not. Absent when the intent was set with plain kubectl annotate.
const (
	PausedByAnnotation              = "pg-ha/paused-by"
	SwitchoverRequestedByAnnotation = "pg-ha/switchover-requested-by"
)

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
	// PausedBy is the control-API client that requested the pause (PausedByAnnotation);
	// "" when it was set with kubectl or not set at all. Provenance for the API's
	// status response -- no agent logic reads it.
	PausedBy string
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
		PausedBy:         strings.TrimSpace(cm.Annotations[PausedByAnnotation]),
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
	// Monotonicity is enforced HERE as well as by the callers, because this Get already
	// holds the recorded highwater and the callers' own guard can be fed a lie. Every
	// advanceMarker call site decides from Observation.Marker, which observe() leaves at
	// its ZERO VALUE (Present=false, i.e. "no constraint") whenever the fence-bounded
	// ReadMarker missed its deadline -- and finishInitdbNative passes MarkerState{}
	// outright. So one apiserver blip on a node whose PGDATA was just rebuilt is enough
	// for shouldAdvanceMarker to wave through timeline 1 over a recorded 7, which defeats
	// the unsafeToServe highwater guard on every stale node in the cluster. A read-modify-
	// write that refuses to lower it costs nothing and cannot be fooled that way. An
	// unparseable recorded value is treated as no constraint, matching shouldAdvanceMarker.
	if cur, ok := cm.Data["timeline"]; ok {
		if v, perr := strconv.ParseUint(cur, 10, 32); perr == nil && timeline < uint32(v) {
			return fmt.Errorf("refusing to lower the highwater marker %s from timeline %d to %d: it is monotonic (#125)", name, v, timeline)
		}
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

// annotateMarker applies set (values) and unset (keys) to the marker ConfigMap's
// annotations in one read-modify-write. It is the single write path for the pause
// and switchover intents, so both the API and the reconcile loop's one-shot clear
// behave identically.
//
// A MISSING marker is an error, not an implicit create: the marker's Data carries
// the highwater timeline, and creating an empty one to hold an annotation would make
// it Present-but-Malformed, which every reader deliberately fails closed on (#174).
// A cluster with no marker has not elected a primary yet and has nothing to pause.
func (c *Client) annotateMarker(ctx context.Context, name string, set map[string]string, unset []string) error {
	cms := c.cs.CoreV1().ConfigMaps(c.namespace)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("marker %s does not exist yet: the cluster has not recorded a primary, so there is nothing to annotate", name)
	}
	if err != nil {
		return fmt.Errorf("get marker %s: %w", name, err)
	}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	for k, v := range set {
		cm.Annotations[k] = v
	}
	for _, k := range unset {
		delete(cm.Annotations, k)
	}
	if _, uerr := cms.Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
		return fmt.Errorf("annotate marker %s: %w", name, uerr)
	}
	return nil
}

// SetPause sets or clears maintenance mode on the marker, recording requestedBy when
// pausing (and dropping it when resuming, so a stale requester cannot outlive the
// pause it describes). Idempotent: pausing an already-paused cluster rewrites the
// same value.
func (c *Client) SetPause(ctx context.Context, name string, on bool, requestedBy string) error {
	if !on {
		return c.annotateMarker(ctx, name, nil, []string{PauseAnnotation, PausedByAnnotation})
	}
	set := map[string]string{PauseAnnotation: "true"}
	if requestedBy != "" {
		set[PausedByAnnotation] = requestedBy
	}
	return c.annotateMarker(ctx, name, set, []string{})
}

// SetSwitchoverTarget requests a controlled handoff to target. The serving primary
// still decides WHEN (only once target is a caught-up, same-timeline standby) and
// clears the annotation itself, so this only records the request.
func (c *Client) SetSwitchoverTarget(ctx context.Context, name, target, requestedBy string) error {
	set := map[string]string{SwitchoverTargetAnnotation: target}
	if requestedBy != "" {
		set[SwitchoverRequestedByAnnotation] = requestedBy
	}
	return c.annotateMarker(ctx, name, set, []string{})
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
	_, hasTarget := cm.Annotations[SwitchoverTargetAnnotation]
	_, hasRequester := cm.Annotations[SwitchoverRequestedByAnnotation]
	if !hasTarget && !hasRequester {
		return nil
	}
	// Drop the requester with the request: a leftover pg-ha/switchover-requested-by
	// would attribute the NEXT switchover to whoever asked for the last one.
	delete(cm.Annotations, SwitchoverTargetAnnotation)
	delete(cm.Annotations, SwitchoverRequestedByAnnotation)
	if _, uerr := cms.Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
		return fmt.Errorf("clear switchover annotation on %s: %w", name, uerr)
	}
	return nil
}
