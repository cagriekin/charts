package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStatefulSetReplicasReadsSpecReplicas(t *testing.T) {
	n := int32(3)
	cs := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: ns},
		Spec:       appsv1.StatefulSetSpec{Replicas: &n},
	})
	got, err := NewWithClient(cs, ns).StatefulSetReplicas(context.Background(), "pg")
	if err != nil || got != 3 {
		t.Fatalf("got %d, %v; want 3", got, err)
	}
}

// An unset spec.replicas is the API default of one, not zero: zero would make the agent
// probe no peers and pre-create no slots on a StatefulSet that does have a pod.
func TestStatefulSetReplicasNilMeansOne(t *testing.T) {
	cs := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: ns},
	})
	got, err := NewWithClient(cs, ns).StatefulSetReplicas(context.Background(), "pg")
	if err != nil || got != 1 {
		t.Fatalf("got %d, %v; want 1", got, err)
	}
}

// A missing StatefulSet (or a denied Get) is an error the caller falls back on, named so
// the log says which object could not be read.
func TestStatefulSetReplicasMissingIsAnError(t *testing.T) {
	_, err := NewWithClient(fake.NewSimpleClientset(), ns).StatefulSetReplicas(context.Background(), "pg")
	if err == nil || !strings.Contains(err.Error(), "get statefulset pg") {
		t.Fatalf("err = %v; want a named get error", err)
	}
}
