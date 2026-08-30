package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const ns = "ns"

func TestPatchWriteSelectorIsIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: ns},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{podNameSelectorKey: "pg-0"}},
	})
	c := NewWithClient(cs, ns)
	ctx := context.Background()

	changed, err := c.PatchWriteSelector(ctx, "pg", "pg-1")
	if err != nil || !changed {
		t.Fatalf("first patch: changed=%v err=%v", changed, err)
	}
	svc, _ := cs.CoreV1().Services(ns).Get(ctx, "pg", metav1.GetOptions{})
	if svc.Spec.Selector[podNameSelectorKey] != "pg-1" {
		t.Errorf("selector = %q, want pg-1", svc.Spec.Selector[podNameSelectorKey])
	}
	changed, err = c.PatchWriteSelector(ctx, "pg", "pg-1")
	if err != nil || changed {
		t.Errorf("second patch should be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestReconcilePodLabelsLeavesUnlistedUntouched(t *testing.T) {
	mk := func(name, role string) *corev1.Pod {
		labels := map[string]string{"app.kubernetes.io/component": "postgresql"}
		if role != "" {
			labels["pg-role"] = role
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	}
	cs := fake.NewSimpleClientset(mk("pg-0", "standby"), mk("pg-1", ""), mk("pg-2", "standby"))
	c := NewWithClient(cs, ns)
	ctx := context.Background()

	// pg-0 -> primary, pg-1 -> standby; pg-2 omitted (unreachable -> untouched).
	desired := map[string]string{"pg-0": "primary", "pg-1": "standby"}
	if err := c.ReconcilePodLabels(ctx, "app.kubernetes.io/component=postgresql", desired); err != nil {
		t.Fatal(err)
	}
	get := func(name string) string {
		p, _ := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		return p.Labels["pg-role"]
	}
	if get("pg-0") != "primary" {
		t.Errorf("pg-0 = %q, want primary", get("pg-0"))
	}
	if get("pg-1") != "standby" {
		t.Errorf("pg-1 = %q, want standby", get("pg-1"))
	}
	if get("pg-2") != "standby" {
		t.Errorf("pg-2 = %q, want standby (untouched)", get("pg-2"))
	}
}

func TestMarkerReadWrite(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewWithClient(cs, ns)
	ctx := context.Background()

	if m, err := c.ReadMarker(ctx, "pg-primary"); err != nil || m.Present {
		t.Fatalf("absent marker: present=%v err=%v", m.Present, err)
	}
	if err := c.WriteMarker(ctx, "pg-primary", "pg-1", 7); err != nil {
		t.Fatal(err)
	}
	m, err := c.ReadMarker(ctx, "pg-primary")
	if err != nil || !m.Present || m.Primary != "pg-1" || !m.TimelineOK || m.Timeline != 7 {
		t.Fatalf("read back: %+v err=%v", m, err)
	}
	if m.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d (stamped on write, Part H4)", m.SchemaVersion, SchemaVersion)
	}
	// Update advances it.
	if err := c.WriteMarker(ctx, "pg-primary", "pg-0", 9); err != nil {
		t.Fatal(err)
	}
	if m, _ := c.ReadMarker(ctx, "pg-primary"); m.Timeline != 9 || m.Primary != "pg-0" {
		t.Errorf("after update: %+v", m)
	}
}

func TestMarkerMalformed(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-primary", Namespace: ns},
		Data:       map[string]string{"primary": "pg-0", "timeline": "not-a-number"},
	})
	c := NewWithClient(cs, ns)
	m, err := c.ReadMarker(context.Background(), "pg-primary")
	if err != nil || !m.Present || !m.Malformed || m.TimelineOK {
		t.Errorf("malformed marker: %+v err=%v", m, err)
	}
}

func TestMarkerLegacySchemaVersion(t *testing.T) {
	// A marker written by the repmgrd-mode service-updater (or an older agent) has
	// no schemaVersion key; it must read back as 0 (== legacy v1), not error.
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-primary", Namespace: ns},
		Data:       map[string]string{"primary": "pg-0", "timeline": "3"},
	})
	m, err := NewWithClient(cs, ns).ReadMarker(context.Background(), "pg-primary")
	if err != nil || !m.Present || m.SchemaVersion != 0 || !m.TimelineOK || m.Timeline != 3 {
		t.Errorf("legacy marker: %+v err=%v", m, err)
	}
}

func TestMarkerSwitchoverTarget(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pg-primary", Namespace: ns,
			Annotations: map[string]string{SwitchoverTargetAnnotation: " pg-1 "},
		},
		Data: map[string]string{"primary": "pg-0", "timeline": "7"},
	})
	c := NewWithClient(cs, ns)
	ctx := context.Background()

	m, err := c.ReadMarker(ctx, "pg-primary")
	if err != nil || m.SwitchoverTarget != "pg-1" {
		t.Fatalf("switchover target = %q err=%v, want trimmed pg-1", m.SwitchoverTarget, err)
	}
	// ClearSwitchoverTarget makes the request one-shot.
	if err := c.ClearSwitchoverTarget(ctx, "pg-primary"); err != nil {
		t.Fatal(err)
	}
	if m2, _ := c.ReadMarker(ctx, "pg-primary"); m2.SwitchoverTarget != "" {
		t.Errorf("after clear, target = %q, want empty", m2.SwitchoverTarget)
	}
	// Clearing an already-absent annotation (or a missing marker) is a no-op.
	if err := c.ClearSwitchoverTarget(ctx, "pg-primary"); err != nil {
		t.Errorf("clear when absent should be nil, got %v", err)
	}
	if err := c.ClearSwitchoverTarget(ctx, "does-not-exist"); err != nil {
		t.Errorf("clear on a missing marker should be nil, got %v", err)
	}
}

func TestMarkerPauseAnnotation(t *testing.T) {
	mk := func(val string) Marker {
		cs := fake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pg-primary", Namespace: ns,
				Annotations: map[string]string{PauseAnnotation: val},
			},
			Data: map[string]string{"primary": "pg-0", "timeline": "7"},
		})
		m, err := NewWithClient(cs, ns).ReadMarker(context.Background(), "pg-primary")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if !mk("true").Paused {
		t.Error(`annotation "true" must set Paused`)
	}
	if !mk(" True ").Paused {
		t.Error("annotation must be trimmed + case-insensitive")
	}
	if mk("false").Paused {
		t.Error(`annotation "false" must not set Paused`)
	}
	// A marker with no annotation (the WriteMarker default) is not paused.
	if m, _ := func() (Marker, error) {
		cs := fake.NewSimpleClientset()
		c := NewWithClient(cs, ns)
		if err := c.WriteMarker(context.Background(), "pg-primary", "pg-0", 7); err != nil {
			t.Fatal(err)
		}
		return c.ReadMarker(context.Background(), "pg-primary")
	}(); m.Paused {
		t.Error("WriteMarker-created marker must not be paused")
	}
}

// --- control-API marker mutations (#276) ---

func markerClient(t *testing.T, annotations map[string]string) (*Client, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-primary", Namespace: ns, Annotations: annotations},
		Data:       map[string]string{"primary": "pg-0", "timeline": "7"},
	})
	return NewWithClient(cs, ns), cs
}

func markerAnnotations(t *testing.T, cs *fake.Clientset) map[string]string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), "pg-primary", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cm.Annotations
}

func TestSetPauseWritesMarkerAndRequester(t *testing.T) {
	c, cs := markerClient(t, nil)
	ctx := context.Background()
	if err := c.SetPause(ctx, "pg-primary", true, "ops-admin"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	a := markerAnnotations(t, cs)
	if a[PauseAnnotation] != "true" || a[PausedByAnnotation] != "ops-admin" {
		t.Errorf("annotations = %v", a)
	}
	m, _ := c.ReadMarker(ctx, "pg-primary")
	if !m.Paused {
		t.Error("the marker the reconcile loop reads must report Paused")
	}
	// The pause is the same object kubectl annotate writes, so the Data (highwater
	// timeline) must survive untouched.
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "pg-primary", metav1.GetOptions{})
	if cm.Data["timeline"] != "7" || cm.Data["primary"] != "pg-0" {
		t.Errorf("marker Data must not be disturbed by an annotation write: %v", cm.Data)
	}
}

// Resuming must drop the requester with the pause: a leftover paused-by would
// describe a pause that is no longer in effect.
func TestSetPauseResumeClearsRequester(t *testing.T) {
	c, cs := markerClient(t, map[string]string{PauseAnnotation: "true", PausedByAnnotation: "ops-admin"})
	if err := c.SetPause(context.Background(), "pg-primary", false, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	a := markerAnnotations(t, cs)
	if _, ok := a[PauseAnnotation]; ok {
		t.Error("pause annotation must be removed on resume")
	}
	if _, ok := a[PausedByAnnotation]; ok {
		t.Errorf("paused-by must be removed on resume: %v", a)
	}
}

func TestSetPauseIsIdempotent(t *testing.T) {
	c, cs := markerClient(t, nil)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := c.SetPause(ctx, "pg-primary", true, "ops-admin"); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
	}
	if markerAnnotations(t, cs)[PauseAnnotation] != "true" {
		t.Error("repeated pause must converge on the same value")
	}
}

// An absent marker means no primary has ever been recorded. Creating one to hold an
// annotation would leave it Present-but-Malformed, which readers fail closed on.
func TestAnnotateMarkerRefusesToCreate(t *testing.T) {
	c := NewWithClient(fake.NewSimpleClientset(), ns)
	ctx := context.Background()
	err := c.SetPause(ctx, "pg-primary", true, "ops-admin")
	if err == nil {
		t.Fatal("pausing without a marker must fail rather than create one")
	}
	if !strings.Contains(err.Error(), "has not recorded a primary") {
		t.Errorf("the error should explain why: %v", err)
	}
	if _, gerr := c.cs.CoreV1().ConfigMaps(ns).Get(ctx, "pg-primary", metav1.GetOptions{}); gerr == nil {
		t.Error("no marker ConfigMap should have been created")
	}
}

func TestSetSwitchoverTargetWritesRequester(t *testing.T) {
	c, cs := markerClient(t, nil)
	ctx := context.Background()
	if err := c.SetSwitchoverTarget(ctx, "pg-primary", "pg-1", "ops-admin"); err != nil {
		t.Fatalf("switchover: %v", err)
	}
	a := markerAnnotations(t, cs)
	if a[SwitchoverTargetAnnotation] != "pg-1" || a[SwitchoverRequestedByAnnotation] != "ops-admin" {
		t.Errorf("annotations = %v", a)
	}
	m, _ := c.ReadMarker(ctx, "pg-primary")
	if m.SwitchoverTarget != "pg-1" {
		t.Errorf("the loop must see the target: %q", m.SwitchoverTarget)
	}
}

// The reconcile loop's one-shot clear must take the requester with it, or the next
// switchover is attributed to whoever asked for the last one.
func TestClearSwitchoverTargetDropsRequester(t *testing.T) {
	c, cs := markerClient(t, map[string]string{
		SwitchoverTargetAnnotation:      "pg-1",
		SwitchoverRequestedByAnnotation: "ops-admin",
	})
	if err := c.ClearSwitchoverTarget(context.Background(), "pg-primary"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	a := markerAnnotations(t, cs)
	if _, ok := a[SwitchoverRequestedByAnnotation]; ok {
		t.Errorf("requester must be cleared with the request: %v", a)
	}
}

// A stale requester left by an older agent (target already gone) must still be
// cleaned up rather than skipped by the early return.
func TestClearSwitchoverTargetCleansOrphanRequester(t *testing.T) {
	c, cs := markerClient(t, map[string]string{SwitchoverRequestedByAnnotation: "ops-admin"})
	if err := c.ClearSwitchoverTarget(context.Background(), "pg-primary"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := markerAnnotations(t, cs)[SwitchoverRequestedByAnnotation]; ok {
		t.Error("an orphaned requester annotation must be removed")
	}
}

// --- #298 review: ReconcilePodLabels must converge every pod it can ---

// mkPod is the shape ReconcilePodLabels lists over.
func mkRolePod(name, role string) *corev1.Pod {
	labels := map[string]string{"app.kubernetes.io/component": "postgresql"}
	if role != "" {
		labels["pg-role"] = role
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
}

// Returning inside the patch loop made convergence depend on WHICH pod failed: a 429
// under load, an admission webhook, or a 409 on a pod being rewritten concurrently
// left every LATER pod on its stale pg-role, and service-readonly.yaml selects on
// exactly that label. A failover in which pg-0's patch fails would leave pg-1 and
// beyond wrongly in (or out of) the read-only Service until a later tick happened to
// succeed on the same first pod.
func TestReconcilePodLabelsConvergesPastAFailedPatch(t *testing.T) {
	cs := fake.NewSimpleClientset(mkRolePod("pg-0", "standby"), mkRolePod("pg-1", "primary"), mkRolePod("pg-2", "primary"))
	cs.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() == "pg-0" {
			return true, nil, apierrors.NewTooManyRequestsError("injected")
		}
		return false, nil, nil
	})
	c := NewWithClient(cs, ns)
	ctx := context.Background()

	desired := map[string]string{"pg-0": "primary", "pg-1": "standby", "pg-2": "standby"}
	err := c.ReconcilePodLabels(ctx, "app.kubernetes.io/component=postgresql", desired)
	if err == nil {
		t.Fatal("the failed patch must still be reported")
	}
	if !strings.Contains(err.Error(), "pg-0") {
		t.Errorf("error should name the pod that could not be labelled: %v", err)
	}
	get := func(name string) string {
		p, _ := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		return p.Labels["pg-role"]
	}
	// The point of the fix: the pods AFTER the failure are converged in the same pass.
	if get("pg-1") != "standby" || get("pg-2") != "standby" {
		t.Errorf("pods after the failed one were abandoned: pg-1=%q pg-2=%q", get("pg-1"), get("pg-2"))
	}
	if get("pg-0") != "standby" {
		t.Errorf("pg-0's label should be untouched by a failed patch, got %q", get("pg-0"))
	}
}

// Every failure is reported, not just the first: an operator reading the log needs to
// know the blast radius, and errors.Join keeps them all.
func TestReconcilePodLabelsReportsEveryFailure(t *testing.T) {
	cs := fake.NewSimpleClientset(mkRolePod("pg-0", "standby"), mkRolePod("pg-1", "standby"))
	cs.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTooManyRequestsError("injected")
	})
	err := NewWithClient(cs, ns).ReconcilePodLabels(context.Background(),
		"app.kubernetes.io/component=postgresql", map[string]string{"pg-0": "primary", "pg-1": "primary"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"pg-0", "pg-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
}

// A pod that vanished between the List and the Patch is not an error: it holds no
// label anyone can read, and the next tick lists without it. Reporting it would make
// an ordinary scale-down or eviction look like a routing failure.
func TestReconcilePodLabelsIgnoresAVanishedPod(t *testing.T) {
	cs := fake.NewSimpleClientset(mkRolePod("pg-0", "standby"), mkRolePod("pg-1", "standby"))
	cs.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() == "pg-1" {
			return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), "pg-1")
		}
		return false, nil, nil
	})
	c := NewWithClient(cs, ns)
	ctx := context.Background()
	if err := c.ReconcilePodLabels(ctx, "app.kubernetes.io/component=postgresql",
		map[string]string{"pg-0": "primary", "pg-1": "standby"}); err != nil {
		t.Fatalf("a NotFound patch must not surface as an error: %v", err)
	}
	p, _ := cs.CoreV1().Pods(ns).Get(ctx, "pg-0", metav1.GetOptions{})
	if p.Labels["pg-role"] != "primary" {
		t.Errorf("pg-0 = %q, want primary", p.Labels["pg-role"])
	}
}

// --- #298 review: the highwater marker is monotonic in WriteMarker itself ---

// Monotonicity was enforced only by the callers, and the callers can be fed a lie:
// every advanceMarker call site decides from Observation.Marker, which observe()
// leaves at its ZERO VALUE (Present=false, i.e. "no constraint") whenever the
// fence-bounded ReadMarker missed its deadline -- and finishInitdbNative passes
// MarkerState{} outright. One apiserver blip on a node whose PGDATA was just rebuilt
// was then enough to record timeline 1 over a recorded 7, defeating the unsafeToServe
// highwater guard on every stale node in the cluster.
func TestWriteMarkerRefusesToLowerTheTimeline(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewWithClient(cs, ns)
	ctx := context.Background()
	if err := c.WriteMarker(ctx, "pg-primary", "pg-0", 7); err != nil {
		t.Fatal(err)
	}
	err := c.WriteMarker(ctx, "pg-primary", "pg-1", 1)
	if err == nil {
		t.Fatal("lowering the recorded timeline must be refused")
	}
	// Both numbers belong in the message: which highwater, and what tried to replace it.
	for _, want := range []string{"7", "1", "monotonic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	m, _ := c.ReadMarker(ctx, "pg-primary")
	if m.Timeline != 7 || m.Primary != "pg-0" {
		t.Errorf("marker was mutated by the refused write: %+v", m)
	}
}

// The guard is `<`, not `<=`: a primary re-recording its own timeline (a new holder on
// the SAME timeline, a periodic refresh) is the ordinary case and must still land.
func TestWriteMarkerAllowsTheSameTimelineAndHigher(t *testing.T) {
	c := NewWithClient(fake.NewSimpleClientset(), ns)
	ctx := context.Background()
	if err := c.WriteMarker(ctx, "pg-primary", "pg-0", 7); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMarker(ctx, "pg-primary", "pg-1", 7); err != nil {
		t.Fatalf("same-timeline primary change must be allowed: %v", err)
	}
	if m, _ := c.ReadMarker(ctx, "pg-primary"); m.Primary != "pg-1" {
		t.Errorf("primary = %q, want pg-1", m.Primary)
	}
	if err := c.WriteMarker(ctx, "pg-primary", "pg-1", 8); err != nil {
		t.Fatalf("advancing must be allowed: %v", err)
	}
	if m, _ := c.ReadMarker(ctx, "pg-primary"); m.Timeline != 8 {
		t.Errorf("timeline = %d, want 8", m.Timeline)
	}
}

// An unparseable recorded value is treated as no constraint, matching
// shouldAdvanceMarker -- otherwise a hand-edited or corrupted ConfigMap would wedge
// the marker permanently, with no way for the agent to repair it.
func TestWriteMarkerTreatsAnUnparseableTimelineAsNoConstraint(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-primary", Namespace: ns},
		Data:       map[string]string{"primary": "pg-0", "timeline": "not-a-number"},
	})
	c := NewWithClient(cs, ns)
	ctx := context.Background()
	if err := c.WriteMarker(ctx, "pg-primary", "pg-1", 3); err != nil {
		t.Fatalf("a corrupt recorded timeline must not wedge the marker: %v", err)
	}
	if m, _ := c.ReadMarker(ctx, "pg-primary"); m.Timeline != 3 {
		t.Errorf("timeline = %d, want 3", m.Timeline)
	}
}
