package egress

import (
	"encoding/json"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
	"time"
)

func TestReplicaStatusRejectsStaleAndOldRevision(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "proxy", UID: "new", Annotations: map[string]string{}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	s := ReplicaStatus{PodUID: "new", DesiredRevision: "r2", LoadedRevision: "r2", State: "loaded", ObservedAt: now.Format(time.RFC3339)}
	check := func(want string) {
		t.Helper()
		b, _ := json.Marshal(s)
		p.Annotations[StatusAnnotation] = string(b)
		if got := ObserveReplica(p, "r2", now); got.State != want {
			t.Fatalf("got %+v want %s", got, want)
		}
	}
	check("loaded")
	s.LoadedRevision = "r1"
	check("syncing")
	s.LoadedRevision = "r2"
	s.PodUID = "old"
	check("syncing")
	s.PodUID = "new"
	s.ObservedAt = now.Add(-time.Minute).Format(time.RFC3339)
	check("failed")
	s.ObservedAt = now.Format(time.RFC3339)
	p.Status.Conditions = nil
	check("syncing")
}
