package egress

import (
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const StatusAnnotation = "workspace.bocom.io/egress-status"

type ReplicaStatus struct {
	Pod             string `json:"pod"`
	PodUID          string `json:"podUID"`
	DesiredRevision string `json:"desiredRevision"`
	LoadedRevision  string `json:"loadedRevision"`
	State           string `json:"state"`
	Error           string `json:"error"`
	ObservedAt      string `json:"observedAt"`
}

// ObserveReplica distrusts stale heartbeats, replacement Pods and reports for
// an older policy. A ready Pod alone is not evidence that a policy was loaded.
func ObserveReplica(p corev1.Pod, revision string, now time.Time) ReplicaStatus {
	var s ReplicaStatus
	if err := json.Unmarshal([]byte(p.Annotations[StatusAnnotation]), &s); err != nil {
		return ReplicaStatus{Pod: p.Name, PodUID: string(p.UID), State: "syncing", Error: "Waiting for replica report"}
	}
	s.Pod = p.Name
	if s.PodUID != string(p.UID) {
		s.State = "syncing"
		s.Error = "Report belongs to a different Pod"
		return s
	}
	at, err := time.Parse(time.RFC3339, s.ObservedAt)
	if err != nil || now.Sub(at) > 30*time.Second || at.After(now.Add(5*time.Second)) {
		s.State = "failed"
		s.Error = "Replica report is stale or invalid"
		return s
	}
	if s.State == "failed" {
		return s
	}
	if s.DesiredRevision != revision || s.LoadedRevision != revision {
		s.State = "syncing"
		return s
	}
	ready := false
	for _, condition := range p.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready || p.DeletionTimestamp != nil {
		s.State = "syncing"
		return s
	}
	s.State = "loaded"
	return s
}
