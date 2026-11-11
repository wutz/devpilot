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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// CommitConsistency selects how the snapshot is made consistent.
// +kubebuilder:validation:Enum=Sync;Freeze;OfflineOnly
type CommitConsistency string

const (
	ConsistencySync        CommitConsistency = "Sync"
	ConsistencyFreeze      CommitConsistency = "Freeze"
	ConsistencyOfflineOnly CommitConsistency = "OfflineOnly"
)

// CommitCompression selects the layer compression algorithm.
// +kubebuilder:validation:Enum=zstd;gzip
type CommitCompression string

const (
	CompressionZstd CommitCompression = "zstd"
	CompressionGzip CommitCompression = "gzip"
)

// CommitPhase is the lifecycle phase of a DevBoxCommit.
// +kubebuilder:validation:Enum=Pending;Snapshotting;Building;Pushing;Succeeded;Failed
type CommitPhase string

const (
	CommitPhasePending      CommitPhase = "Pending"
	CommitPhaseSnapshotting CommitPhase = "Snapshotting"
	CommitPhaseBuilding     CommitPhase = "Building"
	CommitPhasePushing      CommitPhase = "Pushing"
	CommitPhaseSucceeded    CommitPhase = "Succeeded"
	CommitPhaseFailed       CommitPhase = "Failed"
)

// CommitMode records which source path the commit used.
// +kubebuilder:validation:Enum="";OfflinePVC;OnlineSnapshot;OnlineSameNode
type CommitMode string

const (
	CommitModeOfflinePVC     CommitMode = "OfflinePVC"
	CommitModeOnlineSnapshot CommitMode = "OnlineSnapshot"
	CommitModeOnlineSameNode CommitMode = "OnlineSameNode"
)

// DevBoxCommitSpec defines the desired state of DevBoxCommit. The whole spec is
// immutable: every commit is an independent, auditable build record.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="DevBoxCommit spec is immutable"
type DevBoxCommitSpec struct {
	// devBoxName is the source DevBox in this namespace.
	// +kubebuilder:validation:MinLength=1
	DevBoxName string `json:"devBoxName"`

	// destination is the registry repository and tags to push.
	Destination CommitDestination `json:"destination"`

	// consistency selects the snapshot strategy.
	// +optional
	// +kubebuilder:default=Sync
	Consistency CommitConsistency `json:"consistency,omitempty"`

	// compression is the layer compression algorithm.
	// +optional
	// +kubebuilder:default=zstd
	Compression CommitCompression `json:"compression,omitempty"`

	// cleanPackageCache strips apt archives and pip http caches.
	// +optional
	// +kubebuilder:default=true
	CleanPackageCache bool `json:"cleanPackageCache,omitempty"`

	// excludePaths are extra upper-relative globs to skip.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	ExcludePaths []string `json:"excludePaths,omitempty"`

	// changes are config overlays applied on top of the base image config.
	// +optional
	Changes CommitChanges `json:"changes,omitempty"`

	// rebaseDevBox switches the devbox base to the new image and clears upper/
	// on the next start. Only allowed for offline commits.
	// +optional
	RebaseDevBox bool `json:"rebaseDevBox,omitempty"`
}

// CommitDestination is a registry repository plus tags.
type CommitDestination struct {
	// repository is registry/project/repo without a tag.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// tags are pushed for the resulting manifest.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	Tags []string `json:"tags,omitempty"`
}

// CommitChanges overlays OCI config values.
type CommitChanges struct {
	// author sets the config author.
	// +optional
	Author string `json:"author,omitempty"`

	// comment sets the config comment.
	// +optional
	Comment string `json:"comment,omitempty"`

	// env entries are appended.
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []NameValue `json:"env,omitempty"`

	// workingDir replaces the config working directory.
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`

	// cmd replaces the config cmd.
	// +optional
	Cmd []string `json:"cmd,omitempty"`
}

// NameValue is an environment entry.
type NameValue struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +optional
	Value string `json:"value,omitempty"`
}

// DevBoxCommitStatus defines the observed state of DevBoxCommit.
type DevBoxCommitStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Phase CommitPhase `json:"phase,omitempty"`

	// +optional
	Mode CommitMode `json:"mode,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	BaseImageDigest string `json:"baseImageDigest,omitempty"`

	// +optional
	LayerDiffID string `json:"layerDiffID,omitempty"`

	// +optional
	LayerSize int64 `json:"layerSize,omitempty"`

	// digest is the new manifest digest.
	// +optional
	Digest string `json:"digest,omitempty"`

	// images are the fully qualified pushed references.
	// +optional
	Images []string `json:"images,omitempty"`

	// snapshotName is the temporary VolumeSnapshot, cleaned up afterwards.
	// +optional
	SnapshotName string `json:"snapshotName,omitempty"`

	// buildJobName is the isolated build job.
	// +optional
	BuildJobName string `json:"buildJobName,omitempty"`

	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// conditions represent the current state of the DevBoxCommit resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DevBoxCommit is the Schema for the devboxcommits API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="DevBox",type=string,JSONPath=`.spec.devBoxName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.status.mode`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.status.digest`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DevBoxCommit struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DevBoxCommit
	// +required
	Spec DevBoxCommitSpec `json:"spec"`

	// status defines the observed state of DevBoxCommit
	// +optional
	Status DevBoxCommitStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DevBoxCommitList contains a list of DevBoxCommit
type DevBoxCommitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DevBoxCommit `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DevBoxCommit{}, &DevBoxCommitList{})
		return nil
	})
}

// Active reports whether the commit still holds the per-DevBox commit lock.
func (c *DevBoxCommit) Active() bool {
	switch c.Status.Phase {
	case CommitPhaseSnapshotting, CommitPhaseBuilding, CommitPhasePushing:
		return true
	}
	return false
}
