package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
