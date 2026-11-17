/*
Copyright 2026 wutz.

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ImagePrewarmPhase summarises the rollout progress across matching nodes.
// +kubebuilder:validation:Enum=Pending;Running;Completed;Degraded
type ImagePrewarmPhase string

const (
	ImagePrewarmPhasePending   ImagePrewarmPhase = "Pending"
	ImagePrewarmPhaseRunning   ImagePrewarmPhase = "Running"
	ImagePrewarmPhaseCompleted ImagePrewarmPhase = "Completed"
	ImagePrewarmPhaseDegraded  ImagePrewarmPhase = "Degraded"
)

// ImagePrewarmSpec defines the desired state of ImagePrewarm.
type ImagePrewarmSpec struct {
	// image is the container image reference to pre-pull across matching nodes.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// nodeSelector restricts pre-warming to nodes whose labels match.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations are attached to the pre-warm Pods so tainted GPU nodes can
	// be warmed.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// imagePullSecrets references pull secrets in the pre-warm namespace for
	// private registries.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// maxConcurrency caps the number of nodes pulling the image simultaneously.
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=200
	MaxConcurrency int32 `json:"maxConcurrency,omitempty"`

	// pinImage keeps a minimal 0-GPU sleep sentinel Pod running per node so
	// kubelet image GC does not evict the pre-warmed image layers.
	// +optional
	PinImage bool `json:"pinImage,omitempty"`

	// ttlHoursAfterComplete automatically deletes this ImagePrewarm after it
	// has been Completed for the given hours. 0 disables expiration.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLHoursAfterComplete int32 `json:"ttlHoursAfterComplete,omitempty"`
}

// ImagePrewarmStatus defines the observed state of ImagePrewarm.
type ImagePrewarmStatus struct {
	// observedGeneration is the last spec generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is the rollout phase.
	// +optional
	Phase ImagePrewarmPhase `json:"phase,omitempty"`

	// resolvedDigest is the immutable image digest resolved from spec.image.
	// +optional
	ResolvedDigest string `json:"resolvedDigest,omitempty"`

	// desiredNodes is the number of eligible nodes matching nodeSelector and tolerations.
	// +optional
	DesiredNodes int32 `json:"desiredNodes,omitempty"`

	// warmedNodes is the number of desired nodes where the image is cached.
	// +optional
	WarmedNodes int32 `json:"warmedNodes,omitempty"`

	// inProgressNodes is the number of nodes currently running a pre-warm Pod.
	// +optional
	InProgressNodes int32 `json:"inProgressNodes,omitempty"`

	// failedNodes is the number of desired nodes whose pre-warm Pod failed.
	// +optional
	FailedNodes int32 `json:"failedNodes,omitempty"`

	// message summarises the current rollout state.
	// +optional
	Message string `json:"message,omitempty"`

	// completedAt records when the rollout first reached Completed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// conditions represent the current state of the ImagePrewarm resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ipw
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredNodes`
// +kubebuilder:printcolumn:name="Warmed",type=integer,JSONPath=`.status.warmedNodes`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedNodes`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ImagePrewarm is the Schema for the imageprewarms API.
type ImagePrewarm struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ImagePrewarm
	// +required
	Spec ImagePrewarmSpec `json:"spec"`

	// status defines the observed state of ImagePrewarm
	// +optional
	Status ImagePrewarmStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ImagePrewarmList contains a list of ImagePrewarm
type ImagePrewarmList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ImagePrewarm `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ImagePrewarm{}, &ImagePrewarmList{})
		return nil
	})
}
