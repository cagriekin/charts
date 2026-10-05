package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// AcceptLagAnnotation, set to a pod name on the marker ConfigMap, is the operator's
// explicit acceptance of the data loss the #273 lag gate would otherwise refuse: that pod
// may promote however far behind the recorded position it is. It waives the gate for the
// named node only, and only once that node is the lease holder the election settled on
// (the most-advanced reachable standby -- which is also the node whose refusal is being
// logged, and the refusal prints the exact annotate command). Naming a further-behind node
// is ignored: letting it jump the ranking would discard even more WAL than the gate
// refused. One-shot: cleared by whichever node next serves read-write, so an acceptance
// left over from an episode that ended another way (the primary came back) can never
// waive the bound on a later failover. Deliberately NOT the switchover-target annotation:
// that one is a routine handoff request whose target must be caught up, and a pending one
// must never double as a waiver of the RPO bound when the primary happens to die before
// the target catches up. Set with
// `kubectl annotate configmap <fullname>-primary pg-ha/accept-failover-lag=<pod>`.
const AcceptLagAnnotation = "pg-ha/accept-failover-lag"

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
	// AcceptLagTarget is the pod named by AcceptLagAnnotation ("" if none), #273.
	AcceptLagTarget string
	// LSN is the serving primary's last recorded write position (PostgreSQL text form
	// "X/Y"), "" when none was ever recorded (#273). It is the reference the lag gate
	// compares a failover candidate against: the marker outlives the primary's pod, which
	// its gossip annotation does not, so it is the one place that still says where the
	// primary was once the primary is gone. Refreshed every primary tick only while the
	// gate is enabled -- paused or not, since recording is observation, not action -- so
	// barring apiserver errors it is at most one reconcile interval behind the true
	// position, and the lag it yields is a floor, not a ceiling. Dropped by WriteMarker on a
	// timeline advance: a position from the previous timeline must never be compared on the
	// next one. Parsed by the agent (pg.ParseLSN), not here.
	LSN string
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
		AcceptLagTarget:  strings.TrimSpace(cm.Annotations[AcceptLagAnnotation]),
		LSN:              strings.TrimSpace(cm.Data["lsn"]),
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
		v, perr := strconv.ParseUint(cur, 10, 32)
		if perr == nil && timeline < uint32(v) {
			return fmt.Errorf("refusing to lower the highwater marker %s from timeline %d to %d: it is monotonic (#125)", name, v, timeline)
		}
		// A timeline ADVANCE retires the recorded position (#273): it was measured on the
		// previous timeline, and the lag gate only ever compares same-timeline positions.
		// Left in place it would be compared against the new timeline the moment that one
		// is recorded as current -- blocking a planned restart against a stale high value,
		// or waving through a real loss against a stale low one.
		if perr != nil || timeline > uint32(v) {
			delete(cm.Data, "lsn")
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

// patchMarker applies a JSON merge patch to the marker ConfigMap. A MISSING marker is a
// no-op, not a create: the marker's identity is the highwater timeline WriteMarker records,
// and creating one here would make it Present-but-Malformed, which every reader fails
// closed on (#174). A merge patch is one API call with no read-modify-write, so it cannot
// 409 against -- or make a 409 for -- the control API's annotateMarker or the Promote
// branch's WriteMarker, which matters once the lag gate writes the marker every busy tick.
func (c *Client) patchMarker(ctx context.Context, name string, pt types.PatchType, patch []byte, what string) error {
	_, err := c.cs.CoreV1().ConfigMaps(c.namespace).Patch(ctx, name, pt, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s on marker %s: %w", what, name, err)
	}
	return nil
}

// WriteMarkerLSN records the serving primary's write position in the marker's Data (key
// "lsn", PostgreSQL "X/Y" text) for the #273 lag gate. The caller skips the call when the
// marker it just observed already carries the position, so this is one patch per change.
// The highwater, the primary name and every annotation are untouched.
//
// The write is FENCED to the marker state the position was measured under: an RFC 6902 JSON
// patch (application/json-patch+json, NOT a merge patch) whose test ops require data.primary == primary and data.timeline == timeline before the
// add. Without it, a patch still in flight from a primary that has just lost the lease (the
// record runs off the fence budget, not under the tick's operation lock) could land AFTER
// the successor's WriteMarker advanced the timeline and retired the old position, putting a
// previous-timeline LSN back on the marker under the new timeline -- exactly the stale
// same-timeline comparison the retirement exists to prevent (#273 review). A failed test
// op comes back 422 Invalid and means "the marker moved on"; that is the fence doing its
// job; it is reported as ErrMarkerMoved so the caller can say so without treating it as
// an apiserver failure. An absent marker is a nil no-op.
func (c *Client) WriteMarkerLSN(ctx context.Context, name, primary string, timeline uint32, lsn string) error {
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/data/primary", "value": primary},
		{"op": "test", "path": "/data/timeline", "value": strconv.FormatUint(uint64(timeline), 10)},
		{"op": "add", "path": "/data/lsn", "value": lsn},
	})
	if err != nil {
		return err
	}
	err = c.patchMarker(ctx, name, types.JSONPatchType, patch, "record position")
	if apierrors.IsInvalid(err) {
		return fmt.Errorf("%w: %v", ErrMarkerMoved, err)
	}
	return err
}

// ErrMarkerMoved is returned by WriteMarkerLSN when the marker no longer names the primary
// and timeline the position was measured under, so the fenced write was refused (#273).
var ErrMarkerMoved = errors.New("marker names another primary or timeline; position not recorded")

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

// ClearAcceptLagTarget removes the #273 lag-acceptance annotation, making it one-shot:
// called by whichever node serves read-write while it is set, so neither the node it named
// (after promoting) nor a returning primary leaves it behind to waive a later failover's
// bound. A merge patch with a null value deletes the key; a missing marker or an absent
// annotation is a no-op.
func (c *Client) ClearAcceptLagTarget(ctx context.Context, name string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{AcceptLagAnnotation: nil}}})
	if err != nil {
		return err
	}
	return c.patchMarker(ctx, name, types.MergePatchType, patch, "clear "+AcceptLagAnnotation)
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
