package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func mkPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/component": "postgresql"},
	}}
}

func TestPublishAndReadStatusRoundTrip(t *testing.T) {
	cs := fake.NewSimpleClientset(mkPod("pg-0"), mkPod("pg-1"))
	c := NewWithClient(cs, ns)
	ctx := context.Background()

	want := NodeStatus{Timeline: 7, TimelineOK: true, LSNHi: 0x16, LSNLo: 0xB374D848, LSNOK: true, UpdatedAtUnix: 1_700_000_000}
	if err := c.PublishStatus(ctx, "pg-1", want); err != nil {
		t.Fatalf("publish: %v", err)
	}
	want.SchemaVersion = SchemaVersion // PublishStatus stamps the current schema (Part H4)

	// pg-0 reads peers (excludes itself); pg-1's status must round-trip.
	got, err := c.ReadPeerStatuses(ctx, "app.kubernetes.io/component=postgresql", "pg-0")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	st, ok := got["pg-1"]
	if !ok {
		t.Fatalf("pg-1 status missing; got %v", got)
	}
	if st != want {
		t.Errorf("round-trip = %+v, want %+v", st, want)
	}
}

func TestReadPeerStatusesExcludesSelfAndUngossiped(t *testing.T) {
	cs := fake.NewSimpleClientset(mkPod("pg-0"), mkPod("pg-1"), mkPod("pg-2"))
	c := NewWithClient(cs, ns)
	ctx := context.Background()

	// pg-0 and pg-2 publish; pg-1 never does.
	_ = c.PublishStatus(ctx, "pg-0", NodeStatus{Timeline: 5, TimelineOK: true, UpdatedAtUnix: 1})
	_ = c.PublishStatus(ctx, "pg-2", NodeStatus{Timeline: 5, TimelineOK: true, UpdatedAtUnix: 1})

	got, err := c.ReadPeerStatuses(ctx, "app.kubernetes.io/component=postgresql", "pg-0")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, ok := got["pg-0"]; ok {
		t.Error("self (pg-0) must be excluded")
	}
	if _, ok := got["pg-1"]; ok {
		t.Error("pg-1 never gossiped; must be absent")
	}
	if _, ok := got["pg-2"]; !ok {
		t.Error("pg-2 gossiped; must be present")
	}
}

// ListPodNames is deliberately NOT ReadPeerStatuses: that one filters to pods carrying
// a parseable status annotation, which is the wrong question for "does this pod exist".
// A pod mid-restart, still starting, or that has never published gossip is absent from
// that map but very much present in the cluster -- and treating it as gone is how a
// replication slot that is still needed gets reclaimed (#289).
func TestListPodNamesIncludesPodsWithNoGossip(t *testing.T) {
	withStatus := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pg-0", Namespace: ns,
		Labels:      map[string]string{"app.kubernetes.io/component": "postgresql"},
		Annotations: map[string]string{StatusAnnotation: `{"role":"primary"}`},
	}}
	// No annotation at all: starting, mid-restart, or simply not yet gossiping.
	silent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pg-1", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/component": "postgresql"},
	}}
	// An unparseable annotation: ReadPeerStatuses skips it, so it is the same trap.
	garbled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pg-2", Namespace: ns,
		Labels:      map[string]string{"app.kubernetes.io/component": "postgresql"},
		Annotations: map[string]string{StatusAnnotation: "{not json"},
	}}
	c := NewWithClient(fake.NewSimpleClientset(withStatus, silent, garbled), ns)
	names, err := c.ListPodNames(context.Background(), "app.kubernetes.io/component=postgresql")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"pg-0", "pg-1", "pg-2"} {
		if !got[want] {
			t.Errorf("%s missing from the live pod list %v: a still-needed slot would be reclaimed", want, names)
		}
	}
	if len(names) != 3 {
		t.Errorf("names = %v, want exactly the three pods", names)
	}
}

// The selector is honoured: a pod of another component in the same namespace (pgpool,
// an exporter, a backup Job's pod) is not a PostgreSQL peer.
func TestListPodNamesHonoursTheSelector(t *testing.T) {
	pg := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pg-0", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/component": "postgresql"},
	}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pg-pgpool-1", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/component": "pgpool"},
	}}
	c := NewWithClient(fake.NewSimpleClientset(pg, other), ns)
	names, err := c.ListPodNames(context.Background(), "app.kubernetes.io/component=postgresql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "pg-0" {
		t.Errorf("names = %v, want [pg-0]", names)
	}
}

// An empty namespace is an empty list, not an error: a release scaled to zero, or a
// selector that matches nothing yet, is a normal state the caller must be able to read.
func TestListPodNamesReturnsAnEmptyListNotAnError(t *testing.T) {
	names, err := NewWithClient(fake.NewSimpleClientset(), ns).
		ListPodNames(context.Background(), "app.kubernetes.io/component=postgresql")
	if err != nil {
		t.Fatalf("no pods must not be an error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want empty", names)
	}
}
