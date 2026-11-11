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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// AmountPattern matches the fixed-point decimal strings used by the
// trainpilot Money model (e.g. "2.80"). Floats are never used for money.
const AmountPattern = `^[0-9]+(\.[0-9]{1,6})?$`

// DesiredState is the user-controlled power state of a DevBox.
// +kubebuilder:validation:Enum=Running;Stopped
type DesiredState string

const (
	DesiredStateRunning DesiredState = "Running"
	DesiredStateStopped DesiredState = "Stopped"
)

// BillingMode is immutable for the lifetime of a DevBox.
// +kubebuilder:validation:Enum=OnDemand;Spot
type BillingMode string

const (
	BillingModeOnDemand BillingMode = "OnDemand"
	BillingModeSpot     BillingMode = "Spot"
)

// DevBoxPhase is the high level lifecycle phase.
// +kubebuilder:validation:Enum=Pending;Starting;Running;Stopping;Stopped;Interrupted;Failed
type DevBoxPhase string

const (
	PhasePending     DevBoxPhase = "Pending"
	PhaseStarting    DevBoxPhase = "Starting"
	PhaseRunning     DevBoxPhase = "Running"
	PhaseStopping    DevBoxPhase = "Stopping"
	PhaseStopped     DevBoxPhase = "Stopped"
	PhaseInterrupted DevBoxPhase = "Interrupted"
	PhaseFailed      DevBoxPhase = "Failed"
)

// StartingStep reports granular progress while phase is Starting.
// +kubebuilder:validation:Enum="";AllocatingResources;MountingStorage;PullingImage;StartingContainers
type StartingStep string

const (
	StartingStepNone                StartingStep = ""
	StartingStepAllocatingResources StartingStep = "AllocatingResources"
	StartingStepMountingStorage     StartingStep = "MountingStorage"
	StartingStepPullingImage        StartingStep = "PullingImage"
	StartingStepStartingContainers  StartingStep = "StartingContainers"
)

// SegmentEndReason closes a billing segment.
// +kubebuilder:validation:Enum=UserStopped;IdleTimeout;Preempted;NodeFailure;AccountArrears;Released
type SegmentEndReason string

const (
	EndReasonUserStopped    SegmentEndReason = "UserStopped"
	EndReasonIdleTimeout    SegmentEndReason = "IdleTimeout"
	EndReasonPreempted      SegmentEndReason = "Preempted"
	EndReasonNodeFailure    SegmentEndReason = "NodeFailure"
	EndReasonAccountArrears SegmentEndReason = "AccountArrears"
	EndReasonReleased       SegmentEndReason = "Released"
)

// DevBoxSpec defines the desired state of DevBox.
type DevBoxSpec struct {
	// displayName is a human readable name shown in consoles.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// desiredState is the requested power state.
	// +optional
	// +kubebuilder:default=Running
	DesiredState DesiredState `json:"desiredState,omitempty"`

	// billingMode is immutable after creation (ai-prd §6.2).
	// +optional
	// +kubebuilder:default=OnDemand
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="billingMode is immutable after creation"
	BillingMode BillingMode `json:"billingMode,omitempty"`

	// pricePerHour is a fixed-point CNY amount per hour, e.g. "2.80".
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	PricePerHour string `json:"pricePerHour,omitempty"`

	// image is the base image. The resolved immutable digest is pinned on first
	// start into status.activeImageDigest.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// command overrides the image ENTRYPOINT/CMD. Empty inherits them; when the
	// image has no long running command the container falls back to sleep infinity.
	// +optional
	Command []string `json:"command,omitempty"`

	// args overrides the image CMD when command is set.
	// +optional
	Args []string `json:"args,omitempty"`

	// env is appended to the container environment. DEVPILOT_* names are reserved.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:XValidation:rule="self.all(e, !e.name.startsWith('DEVPILOT_'))",message="spec.env must not override reserved DEVPILOT_* variables"
	Env []corev1.EnvVar `json:"env,omitempty"`

	// resources describes the compute shape of the DevBox.
	// +optional
	Resources DevBoxResources `json:"resources,omitempty"`

	// zone optionally constrains scheduling to a site/zone label value.
	// +optional
	Zone string `json:"zone,omitempty"`

	// rootDisk configures the persistent overlay upperdir block volume.
	RootDisk RootDiskSpec `json:"rootDisk"`

	// volumes are extra shared data mounts; they are never captured by a commit.
	// +optional
	// +listType=map
	// +listMapKey=name
	Volumes []DevBoxVolume `json:"volumes,omitempty"`

	// access configures SSH, Jupyter and custom web ports.
	// +optional
	Access DevBoxAccess `json:"access,omitempty"`

	// lifecycle configures idle shutdown and runtime limits.
	// +optional
	Lifecycle DevBoxLifecycle `json:"lifecycle,omitempty"`

	// nodeSelector is merged into the pod scheduling requirements.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations are passed through to the pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// DevBoxResources describes the compute shape of a DevBox.
type DevBoxResources struct {
	// gpus is the number of GPUs; 0 means a CPU only devbox.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=64
	GPUs int32 `json:"gpus,omitempty"`

	// gpuModel selects GPU nodes by label value (scclabs.dev/gpu by default).
	// +optional
	GPUModel string `json:"gpuModel,omitempty"`

	// cpu is the CPU request/limit quantity, e.g. "8".
	// +optional
	// +kubebuilder:default="4"
	CPU resource.Quantity `json:"cpu,omitempty"`

	// memory is the memory request/limit quantity, e.g. "32Gi".
	// +optional
	// +kubebuilder:default="8Gi"
	Memory resource.Quantity `json:"memory,omitempty"`

	// shmRatio is the fraction of memory given to /dev/shm, e.g. "0.5".
	// +optional
	// +kubebuilder:default="0.5"
	// +kubebuilder:validation:Pattern=`^(0(\.[0-9]+)?|1(\.0+)?)$`
	ShmRatio string `json:"shmRatio,omitempty"`
}

// RootDiskSpec configures the overlay root disk.
// +kubebuilder:validation:XValidation:rule="self.storageClassName == oldSelf.storageClassName",message="rootDisk.storageClassName is immutable"
// +kubebuilder:validation:XValidation:rule="quantity(self.size).compareTo(quantity(oldSelf.size)) >= 0",message="rootDisk.size may only grow"
type RootDiskSpec struct {
	// size is the volume size; online expansion is supported, shrinking is not.
	// +kubebuilder:validation:XIntOrString
	Size resource.Quantity `json:"size"`

	// storageClassName must be an RWO block storage class such as block-nvme.
	// +kubebuilder:validation:MinLength=1
	StorageClassName string `json:"storageClassName"`

	// volumeSnapshotClassName overrides the per-storageclass snapshot class
	// mapping used for online commits.
	// +optional
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`

	// retentionDaysAfterStop releases the devbox after this many days without
	// running. 0 disables automatic release.
	// +optional
	// +kubebuilder:default=15
	// +kubebuilder:validation:Minimum=0
	RetentionDaysAfterStop int32 `json:"retentionDaysAfterStop,omitempty"`
}

// DevBoxVolume is an additional mount attached to the devbox.
type DevBoxVolume struct {
	// name identifies the volume.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// persistentVolumeClaim references an existing claim in the same namespace.
	// +optional
	PersistentVolumeClaim *corev1.PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaim,omitempty"`

	// mountPath is where the volume is mounted inside the devbox root.
	// +kubebuilder:validation:MinLength=1
	MountPath string `json:"mountPath"`

	// readOnly mounts the volume read only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// DevBoxAccess configures the interactive access surface.
type DevBoxAccess struct {
	// ssh configures the in-container SSH server.
	// +optional
	SSH *SSHAccess `json:"ssh,omitempty"`

	// jupyter configures the JupyterLab server.
	// +optional
	Jupyter *JupyterAccess `json:"jupyter,omitempty"`

	// ports exposes extra HTTP ports through the gateway/ingress.
	// +optional
	// +listType=map
	// +listMapKey=name
	Ports []DevBoxPort `json:"ports,omitempty"`
}

// SSHAccess configures SSH.
type SSHAccess struct {
	// enabled turns the in-container sshd on.
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// username is the login user, root by default.
	// +optional
	// +kubebuilder:default=root
	Username string `json:"username,omitempty"`

	// authorizedKeys are injected with a managed-by:devpilot marker.
	// +optional
	// +listType=set
	AuthorizedKeys []string `json:"authorizedKeys,omitempty"`

	// passwordSecretRef enables password login from an existing secret.
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// gatewayRef asks the operator to publish an SSHRoute.
	// +optional
	GatewayRef *NamespacedName `json:"gatewayRef,omitempty"`
}

// JupyterAccess configures JupyterLab.
type JupyterAccess struct {
	// enabled turns JupyterLab on.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// port is the JupyterLab port, 8888 by default.
	// +optional
	// +kubebuilder:default=8888
	Port int32 `json:"port,omitempty"`

	// tokenSecretRef points at an existing token secret; when empty the operator
	// generates and persists one.
	// +optional
	TokenSecretRef *corev1.SecretKeySelector `json:"tokenSecretRef,omitempty"`
}

// DevBoxPort is an extra HTTP port.
type DevBoxPort struct {
	// name identifies the exposed port.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// port is the container port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// protocol is the application protocol; HTTP by default.
	// +optional
	// +kubebuilder:validation:Enum=HTTP;GRPC
	Protocol string `json:"protocol,omitempty"`
}

// DevBoxLifecycle configures lifecycle automation.
type DevBoxLifecycle struct {
	// idleShutdownMinutes shuts the devbox down after this much idle time; 0 disables.
	// +optional
	// +kubebuilder:validation:Minimum=0
	IdleShutdownMinutes int32 `json:"idleShutdownMinutes,omitempty"`

	// maxRuntimeHours stops the devbox after this many hours; 0 means unlimited.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRuntimeHours int32 `json:"maxRuntimeHours,omitempty"`
}

// NamespacedName references an object in a namespace.
type NamespacedName struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// DevBoxEndpoint holds the externally reachable URLs.
type DevBoxEndpoint struct {
	// ssh is the ssh:// gateway URL.
	// +optional
	SSH string `json:"ssh,omitempty"`

	// jupyter is the JupyterLab URL.
	// +optional
	Jupyter string `json:"jupyter,omitempty"`

	// ports maps a spec port name to its public URL.
	// +optional
	Ports map[string]string `json:"ports,omitempty"`
}

// BillingSegment is one running interval, aligned with the trainpilot ledger.
type BillingSegment struct {
	// id is monotonically increasing per DevBox.
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id"`

	// nodeName is the node the segment ran on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// pricePerHour snapshots the price at segment start.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	PricePerHour string `json:"pricePerHour,omitempty"`

	// startTime is when the segment opened.
	StartTime metav1.Time `json:"startTime"`

	// endTime is when the segment closed; empty while running.
	// +optional
	EndTime *metav1.Time `json:"endTime,omitempty"`

	// billedSeconds is the closed duration in seconds.
	// +optional
	BilledSeconds int64 `json:"billedSeconds,omitempty"`

	// cost is the fixed-point amount for this segment.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	Cost string `json:"cost,omitempty"`

	// endReason closes the segment.
	// +optional
	EndReason SegmentEndReason `json:"endReason,omitempty"`
}

// DevBoxStatus defines the observed state of DevBox.
type DevBoxStatus struct {
	// observedGeneration is the last spec generation the controller reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is the lifecycle phase.
	// +optional
	Phase DevBoxPhase `json:"phase,omitempty"`

	// startingStep reports granular progress while phase is Starting.
	// +optional
	StartingStep StartingStep `json:"startingStep,omitempty"`

	// message explains the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// activeImage is the image reference currently in use.
	// +optional
	ActiveImage string `json:"activeImage,omitempty"`

	// activeImageDigest pins the immutable base digest (repo@sha256:...).
	// +optional
	ActiveImageDigest string `json:"activeImageDigest,omitempty"`

	// pendingRebaseImage is applied and upper/ cleared on the next start.
	// +optional
	PendingRebaseImage string `json:"pendingRebaseImage,omitempty"`

	// rootDiskPVC is the generated root disk claim name.
	// +optional
	RootDiskPVC string `json:"rootDiskPVC,omitempty"`

	// rebaseCount counts completed rebases.
	// +optional
	RebaseCount int32 `json:"rebaseCount,omitempty"`

	// podName is the running devbox pod.
	// +optional
	PodName string `json:"podName,omitempty"`

	// nodeName is the node hosting the devbox pod.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// endpoints are the externally reachable URLs.
	// +optional
	Endpoints DevBoxEndpoint `json:"endpoints,omitzero"`

	// stoppedAt records when the devbox last entered Stopped.
	// +optional
	StoppedAt *metav1.Time `json:"stoppedAt,omitempty"`

	// interruptedAt records when the devbox last entered Interrupted.
	// +optional
	InterruptedAt *metav1.Time `json:"interruptedAt,omitempty"`

	// nonRunningSince is the first instant of the current non-running streak.
	// +optional
	NonRunningSince *metav1.Time `json:"nonRunningSince,omitempty"`

	// scheduledReleaseTime is nonRunningSince + retentionDaysAfterStop.
	// +optional
	ScheduledReleaseTime *metav1.Time `json:"scheduledReleaseTime,omitempty"`

	// totalBilledSeconds aggregates closed segments.
	// +optional
	TotalBilledSeconds int64 `json:"totalBilledSeconds,omitempty"`

	// totalCost is the fixed-point aggregate cost.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	TotalCost string `json:"totalCost,omitempty"`

	// segments is the billing ledger, newest last.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Segments []BillingSegment `json:"segments,omitempty"`

	// conditions represent the current state of the DevBox resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DevBox is the Schema for the devboxes API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.desiredState`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.activeImage`,priority=1
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DevBox struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DevBox
	// +required
	Spec DevBoxSpec `json:"spec"`

	// status defines the observed state of DevBox
	// +optional
	Status DevBoxStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DevBoxList contains a list of DevBox
type DevBoxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DevBox `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DevBox{}, &DevBoxList{})
		return nil
	})
}

// NamespacedNameValue returns the referenced namespaced name, defaulting the
// namespace to fallback when unset.
func (n *NamespacedName) NamespacedNameValue(fallback string) types.NamespacedName {
	if n == nil {
		return types.NamespacedName{Namespace: fallback}
	}
	ns := n.Namespace
	if ns == "" {
		ns = fallback
	}
	return types.NamespacedName{Namespace: ns, Name: n.Name}
}
