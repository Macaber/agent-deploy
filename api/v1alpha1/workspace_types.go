/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RepositorySpec defines the Git configuration for initial checkout.
type RepositorySpec struct {
	// URL is the Git repository URL (HTTPS or SSH)
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Branch is the target Git branch to check out
	// +optional
	Branch string `json:"branch,omitempty"`

	// Commit is the target Git commit hash to check out
	// +optional
	Commit string `json:"commit,omitempty"`

	// SecretRef references a Secret containing Git auth credentials (SSH key or token)
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

// VolumeMount defines a directory mount point inside the container mapping to the workspace storage.
type VolumeMount struct {
	// MountPath is the path inside the container where the volume should be mounted
	// +kubebuilder:validation:Required
	MountPath string `json:"mountPath"`

	// SubPath is the subdirectory within the workspace volume to mount
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// RuntimeSpec specifies the execution environment.
type RuntimeSpec struct {
	// Image is the container image to run for the workspace
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// CPU specifies CPU resource limits/requests, e.g. "500m", "2"
	// +optional
	CPU string `json:"cpu,omitempty"`

	// Memory specifies Memory resource limits/requests, e.g. "512Mi", "2Gi"
	// +optional
	Memory string `json:"memory,omitempty"`

	// Env is a list of environment variables to inject into the container
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// Port specifies a custom container port. If not specified, defaults based on image (e.g. 80 for nginx, 8080 otherwise).
	// +optional
	Port int32 `json:"port,omitempty"`

	// Command specifies custom container startup command (entrypoint)
	// +optional
	Command []string `json:"command,omitempty"`

	// Args specifies custom container startup arguments
	// +optional
	Args []string `json:"args,omitempty"`

	// VolumeMounts specifies custom directory mounts inside the container mapping to the workspace storage
	// +optional
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// PostStartScript is a multiline shell script executed as a postStart lifecycle hook immediately after container start
	// +optional
	PostStartScript string `json:"postStartScript,omitempty"`

	// HealthPath is the HTTP path for readiness probe check (e.g. "/health"). If empty, defaults to TCP socket check.
	// +optional
	HealthPath string `json:"healthPath,omitempty"`
}

// StorageSpec defines the storage properties for user persistence.
type StorageSpec struct {
	// Size specifies PVC size, e.g., "10Gi"
	// +kubebuilder:validation:Required
	Size string `json:"size"`

	// StorageClass specifies the K8s StorageClass to request
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

// EnvVar represents a simple environment variable key-value pair.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// WorkspaceSpec defines the desired state of Workspace
type WorkspaceSpec struct {
	// Owner indicates the user ID owning this workspace
	// +kubebuilder:validation:Required
	Owner string `json:"owner"`

	// Repository is the Git repo to clone upon initial startup
	// +optional
	Repository *RepositorySpec `json:"repository,omitempty"`

	// Runtime specifies the container image and resource requirements
	// +kubebuilder:validation:Required
	Runtime RuntimeSpec `json:"runtime"`

	// Storage defines the PVC size and storage class for user files
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// ExposeSSH enables SSH access to the workspace container
	// +optional
	ExposeSSH bool `json:"exposeSSH,omitempty"`

	// IdleTimeout specifies the duration of inactivity after which the workspace scales to 0
	// e.g. "30m", "1h"
	// +optional
	IdleTimeout string `json:"idleTimeout,omitempty"`

	// Stopped indicates whether the workspace is manually stopped (scaled to 0 replicas)
	// +optional
	Stopped bool `json:"stopped,omitempty"`

	// SharedVolumeMounts specifies pre-existing generic/shared PVCs to mount
	// +optional
	SharedVolumeMounts []SharedVolumeMount `json:"sharedVolumeMounts,omitempty"`
}

// SharedVolumeMount defines a mount of a pre-existing generic/shared PVC.
type SharedVolumeMount struct {
	// PVCName is the name of the pre-existing PersistentVolumeClaim to mount
	// +kubebuilder:validation:Required
	PVCName string `json:"pvcName"`

	// MountPath is the path inside the container where the volume should be mounted
	// +kubebuilder:validation:Required
	MountPath string `json:"mountPath"`

	// SubPath is the subdirectory within the shared volume to mount
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// WorkspacePhase defines the state transitions of the Workspace
// +kubebuilder:validation:Enum=Pending;Starting;Running;Sleeping;Stopped;Failed
type WorkspacePhase string

const (
	WorkspacePending  WorkspacePhase = "Pending"
	WorkspaceStarting WorkspacePhase = "Starting"
	WorkspaceRunning  WorkspacePhase = "Running"
	WorkspaceSleeping WorkspacePhase = "Sleeping" // Scaled to 0 due to idle timeout
	WorkspaceStopped  WorkspacePhase = "Stopped"  // Manually stopped
	WorkspaceFailed   WorkspacePhase = "Failed"
)

// WorkspaceStatus defines the observed state of Workspace.
type WorkspaceStatus struct {
	// Phase is the current high-level state of the workspace
	// +optional
	Phase WorkspacePhase `json:"phase,omitempty"`

	// PodName is the name of the active Pod running the workspace runtime
	// +optional
	PodName string `json:"podName,omitempty"`

	// PVCName is the name of the persistent volume claim allocated for the workspace
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// Endpoint is the external URL or access path for the workspace
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// LastActiveTime indicates the timestamp of the last detected activity
	// +optional
	LastActiveTime *metav1.Time `json:"lastActiveTime,omitempty"`

	// ExpiryTime indicates the computed time when this workspace will expire under the TTL policy
	// +optional
	ExpiryTime *metav1.Time `json:"expiryTime,omitempty"`

	// Conditions represent the current state of the Workspace resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Workspace is the Schema for the workspaces API
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec,omitempty"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Workspace{}, &WorkspaceList{})
		return nil
	})
}
