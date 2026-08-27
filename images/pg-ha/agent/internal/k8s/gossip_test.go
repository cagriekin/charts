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
