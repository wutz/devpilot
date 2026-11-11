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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
	"github.com/wutz/devpilot/internal/initroot"
)

// DevBoxCommitReconciler reconciles a DevBoxCommit object. Each commit is one
// immutable build record: the reconciler runs it once and never reopens a
// finished one.
type DevBoxCommitReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
	// Flusher pushes a running DevBox's page cache to its root disk before an
	// online snapshot. Nil means no flush; production wires the exec one.
	Flusher RootDiskFlusher
}

// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxcommits,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxcommits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxcommits/finalizers,verbs=update
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

const (
	// CommitFinalizer guards the build Job and any temporary snapshot until
	// the reconciler has released them.
	CommitFinalizer = "devpilot.wutz.dev/commit"

	// CommitJobSuffix is appended to the commit name for the build Job.
	CommitJobSuffix = "-build"
	// CommitContainerName runs the devpilot-commit binary.
	CommitContainerName = "commit"
	// CommitScratchMount holds the staged layer blob, which must be on a
	// writable filesystem: the image is read-only and the root disk is mounted
	// read-only.
	CommitScratchMount = "/scratch"
	// CommitRegistryMount holds a docker config so the binary can authenticate.
	CommitRegistryMount = "/registry"
	// CommitResultPath is the termination message path the binary writes and
	// the kubelet copies into the container's terminated state.
	CommitResultPath = "/dev/termination-log"
	// CommitJobLabel marks the build Job for convenient selection.
	CommitJobLabel = "devpilot.wutz.dev/commit"

	requeueWhileCommitting = 5 * time.Second
)

// CommitConditionReady summarises whether the image was published.
const CommitConditionReady = "Ready"

// buildResult is the JSON summary devpilot-commit writes to the termination log.
// Every field is optional here so a failure payload, which carries only error,
// decodes into the same struct.
type buildResult struct {
	BaseDigest     string   `json:"baseDigest"`
	ManifestDigest string   `json:"manifestDigest"`
	ConfigDigest   string   `json:"configDigest"`
	DiffID         string   `json:"diffID"`
	LayerSize      int64    `json:"layerSize"`
	Images         []string `json:"images"`
	Error          string   `json:"error"`
}

// Reconcile drives one DevBoxCommit to a terminal phase.
func (r *DevBoxCommitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	commit := &devpilotv1alpha1.DevBoxCommit{}
	if err := r.Get(ctx, req.NamespacedName, commit); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !commit.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, commit)
	}
	if commit.Status.Phase == devpilotv1alpha1.CommitPhaseSucceeded ||
		commit.Status.Phase == devpilotv1alpha1.CommitPhaseFailed {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(commit, CommitFinalizer) {
		controllerutil.AddFinalizer(commit, CommitFinalizer)
		if err := r.Update(ctx, commit); err != nil {
			return ctrl.Result{}, fmt.Errorf("add commit finalizer: %w", err)
		}
	}

	original := commit.DeepCopy()
	if commit.Status.StartTime == nil {
		now := nowUTC()
		commit.Status.StartTime = &now
	}
	commit.Status.ObservedGeneration = commit.Generation

	result, err := r.reconcileCommit(ctx, commit)
	log.V(1).Info("Reconciled DevBoxCommit", "phase", commit.Status.Phase, "mode", commit.Status.Mode)
	if statusErr := r.patchCommitStatus(ctx, original, commit); statusErr != nil {
		return result, statusErr
	}
	return result, err
}

// reconcileCommit advances the commit by one step: pick the source path, start
// the build Job, then read its result.
func (r *DevBoxCommitReconciler) reconcileCommit(ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if r.Config.CommitImage == "" {
		return ctrl.Result{}, r.failCommit(ctx, commit, "CommitDisabled",
			errors.New("no devpilot-commit image is configured; set --devpilot-commit-image"))
	}

	box := &devpilotv1alpha1.DevBox{}
	key := types.NamespacedName{Namespace: commit.Namespace, Name: commit.Spec.DevBoxName}
	if err := r.Get(ctx, key, box); err != nil {
		if isNotFound(err) {
			return ctrl.Result{}, r.failCommit(ctx, commit, "DevBoxNotFound",
				fmt.Errorf("devbox %s does not exist in namespace %s", commit.Spec.DevBoxName, commit.Namespace))
		}
		return ctrl.Result{}, fmt.Errorf("get devbox %s: %w", commit.Spec.DevBoxName, err)
	}

	if busy, err := r.commitInProgress(ctx, commit); err != nil {
		return ctrl.Result{}, err
	} else if busy {
		commit.Status.Phase = devpilotv1alpha1.CommitPhasePending
		commit.Status.Message = fmt.Sprintf("Waiting for an earlier commit of %s to finish", box.Name)
		return ctrl.Result{RequeueAfter: requeueWhileCommitting}, nil
	}

	job, source, waiting := r.buildJobFor(ctx, commit, box)
	if waiting {
		return ctrl.Result{RequeueAfter: requeueWhileSnapshotting}, nil
	}
	if job == nil {
		var err error
		if job, err = r.ensureBuildJob(ctx, commit, box, source); err != nil {
			return ctrl.Result{}, err
		}
		if job == nil {
			// ensureBuildJob recorded the failure on the status instead of
			// starting a Job, so there is nothing to observe.
			return ctrl.Result{}, nil
		}
	}
	log.V(1).Info("Observed commit build Job", "job", job.Name, "succeeded", job.Status.Succeeded, "failed", job.Status.Failed)
	return r.observeBuildJob(ctx, commit, job)
}

// buildJobFor reports the commit's existing build Job plus the volume it reads.
// The second result is set when the source is not available yet, in which case
// the caller only waits.
func (r *DevBoxCommitReconciler) buildJobFor(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox,
) (*batchv1.Job, commitSource, bool) {
	log := logf.FromContext(ctx)

	source, waiting, err := r.resolveSource(ctx, commit, box)
	if err != nil {
		log.Error(err, "Could not prepare the commit source", "commit", commit.Name)
		return nil, commitSource{}, true
	}
	if waiting {
		return nil, commitSource{}, true
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: commit.Namespace, Name: buildJobName(commit)}, job); err == nil {
		return job, source, false
	} else if !isNotFound(err) {
		log.Error(err, "Could not read the commit build Job", "commit", commit.Name)
		return nil, commitSource{}, true
	}
	return nil, source, false
}

// ensureBuildJob creates the isolated build Job that reads the resolved source.
func (r *DevBoxCommitReconciler) ensureBuildJob(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox, source commitSource,
) (*batchv1.Job, error) {
	pvcName := source.PVCName
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: commit.Namespace, Name: pvcName}, pvc); err != nil {
		if isNotFound(err) {
			return nil, r.failCommit(ctx, commit, "RootDiskMissing",
				fmt.Errorf("commit source claim %s of devbox %s does not exist", pvcName, box.Name))
		}
		return nil, fmt.Errorf("get root disk claim %s: %w", pvcName, err)
	}

	job, err := r.buildJob(commit, source)
	if err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil {
		if isAlreadyExists(err) {
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(job), job); getErr != nil {
				return nil, fmt.Errorf("get build job %s: %w", job.Name, getErr)
			}
			return job, nil
		}
		return nil, fmt.Errorf("create build job %s: %w", job.Name, err)
	}

	commit.Status.Phase = devpilotv1alpha1.CommitPhaseBuilding
	commit.Status.BuildJobName = job.Name
	commit.Status.Message = fmt.Sprintf("Building increment layer from the root disk of %s", box.Name)
	r.setCommitCondition(commit, metav1.ConditionFalse, "Building", commit.Status.Message)
	logf.FromContext(ctx).Info("Created commit build Job", "job", job.Name, "rootDiskPVC", pvcName)
	return job, nil
}

// buildJob assembles the isolated build Job. It mounts the source claim
// read-only and gives the container no credentials beyond the registry secret,
// so a compromised commit binary cannot write the source or assume a cluster
// role.
func (r *DevBoxCommitReconciler) buildJob(
	commit *devpilotv1alpha1.DevBoxCommit, source commitSource,
) (*batchv1.Job, error) {
	backoff := int32(0)
	runAsNonRoot := false
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildJobName(commit),
			Namespace: commit.Namespace,
			Labels: map[string]string{
				LabelManagedBy:  ValueManagedBy,
				LabelDevBoxName: commit.Spec.DevBoxName,
				CommitJobLabel:  commit.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelManagedBy:  ValueManagedBy,
						LabelDevBoxName: commit.Spec.DevBoxName,
						CommitJobLabel:  commit.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: new(false),
					// The root disk belongs to the user's own processes, which
					// run as root inside the DevBox; nothing else can read
					// upper/ or .meta/. Containment comes from the read-only
					// mount, the dropped capabilities and the absent token, not
					// from the user ID.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    new(int64(commitUserID)),
						RunAsGroup:   new(int64(commitUserID)),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Volumes:    r.commitVolumes(source.PVCName),
					Containers: []corev1.Container{r.commitContainer(commit)},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(commit, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on build job: %w", err)
	}
	return job, nil
}

// commitUserID is the identity the build runs as. It has to be root: the
// snapshot carries an overlay written by root-owned processes inside the
// DevBox, and devpilot-init leaves its pin in a 0600 .meta/base-image.json, so
// a non-root reader fails on the very first file it opens. The image itself
// still declares USER nonroot; only the pod overrides it.
const commitUserID = 0

func (r *DevBoxCommitReconciler) commitVolumes(pvcName string) []corev1.Volume {
	vols := []corev1.Volume{
		{
			Name: RootDiskVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  true,
				},
			},
		},
		{
			Name:         "scratch",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	if r.Config.RegistrySecret != "" {
		vols = append(vols, corev1.Volume{
			Name: "registry",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: r.Config.RegistrySecret,
					Items: []corev1.KeyToPath{
						{Key: corev1.DockerConfigJsonKey, Path: "config.json"},
					},
				},
			},
		})
	}
	return vols
}

// commitContainer renders the build container and its flags from the spec, so
// the binary never has to read the DevBox API itself.
func (r *DevBoxCommitReconciler) commitContainer(commit *devpilotv1alpha1.DevBoxCommit) corev1.Container {
	spec := commit.Spec
	args := []string{
		"--rootdisk=" + initroot.DefaultRootDisk,
		"--repository=" + spec.Destination.Repository,
		"--tags=" + strings.Join(spec.Destination.Tags, ","),
		"--compression=" + string(spec.Compression),
		fmt.Sprintf("--clean-package-cache=%t", spec.CleanPackageCache),
		"--scratch-dir=" + CommitScratchMount,
		"--result-file=" + CommitResultPath,
	}
	if len(spec.ExcludePaths) > 0 {
		args = append(args, "--exclude="+strings.Join(spec.ExcludePaths, ","))
	}
	if spec.Changes.Author != "" {
		args = append(args, "--author="+spec.Changes.Author)
	}
	if spec.Changes.Comment != "" {
		args = append(args, "--comment="+spec.Changes.Comment)
	}
	if spec.Changes.WorkingDir != "" {
		args = append(args, "--working-dir="+spec.Changes.WorkingDir)
	}
	if env := envPairs(spec.Changes.Env); env != "" {
		args = append(args, "--env="+env)
	}
	if len(spec.Changes.Cmd) > 0 {
		// The CMD is passed as JSON so a shell-quoted rewrite cannot corrupt it.
		if data, err := json.Marshal(spec.Changes.Cmd); err == nil {
			args = append(args, "--cmd="+string(data))
		}
	}

	env := []corev1.EnvVar{
		{Name: "TMPDIR", Value: CommitScratchMount},
		{Name: "HOME", Value: CommitScratchMount},
	}
	mounts := []corev1.VolumeMount{
		{Name: RootDiskVolumeName, MountPath: initroot.DefaultRootDisk, ReadOnly: true},
		{Name: "scratch", MountPath: CommitScratchMount},
	}
	if r.Config.RegistrySecret != "" {
		// go-containerregistry reads DOCKER_CONFIG for its default keychain.
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: CommitRegistryMount})
		mounts = append(mounts, corev1.VolumeMount{
			Name: "registry", MountPath: CommitRegistryMount, ReadOnly: true,
		})
	}

	allowPrivilege := false
	readOnlyRoot := true
	return corev1.Container{
		Name:            CommitContainerName,
		Image:           r.Config.CommitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            args,
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPrivilege,
			ReadOnlyRootFilesystem:   &readOnlyRoot,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{capabilityAll},
				// The scan must read files the user's own processes made
				// private, such as apt's 0700 lists/partial directory. Without
				// this, root is refused like any other user and the build
				// fails with "walk upper dir: permission denied". Read and
				// search only, so writing stays impossible; the source claim
				// is mounted read-only regardless.
				Add: []corev1.Capability{capabilityDACReadSearch},
			},
		},
		VolumeMounts: mounts,
	}
}

// observeBuildJob turns the Job's state into a commit phase. The result comes
// from the container's termination message, which the binary writes before it
// exits, so a failed build never reports a digest.
func (r *DevBoxCommitReconciler) observeBuildJob(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, job *batchv1.Job,
) (ctrl.Result, error) {
	switch {
	case job.Status.Succeeded > 0:
		return ctrl.Result{}, r.finishBuild(ctx, commit, job)
	case job.Status.Failed > 0:
		message := "the build Job failed"
		if detail := r.terminationMessage(ctx, job); detail != "" {
			message = detail
		}
		return ctrl.Result{}, r.failCommit(ctx, commit, "BuildFailed", errors.New(message))
	default:
		commit.Status.Phase = devpilotv1alpha1.CommitPhaseBuilding
		commit.Status.BuildJobName = job.Name
		return ctrl.Result{RequeueAfter: requeueWhileCommitting}, nil
	}
}

// finishBuild records the pulled result. A missing or unparsable message means
// the build is not trustworthy, so it is reported as a failure rather than as a
// success without a digest.
func (r *DevBoxCommitReconciler) finishBuild(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, job *batchv1.Job,
) error {
	raw := r.terminationMessage(ctx, job)
	if raw == "" {
		return r.failCommit(ctx, commit, "ResultMissing",
			errors.New("the build Job reported no termination message"))
	}
	var result buildResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return r.failCommit(ctx, commit, "ResultUnreadable",
			fmt.Errorf("parse the build Job result: %w", err))
	}
	if result.Error != "" {
		return r.failCommit(ctx, commit, "BuildFailed", errors.New(result.Error))
	}
	if result.ManifestDigest == "" || len(result.Images) == 0 {
		return r.failCommit(ctx, commit, "ResultIncomplete",
			errors.New("the build Job result carries no manifest digest"))
	}

	// The image is published, so the temporary clone and snapshot are nothing
	// but wasted storage; releasing them now keeps the namespace prompt even
	// when the commit stays around as a record.
	r.releaseEphemeralBestEffort(ctx, commit)

	now := nowUTC()
	commit.Status.Phase = devpilotv1alpha1.CommitPhaseSucceeded
	commit.Status.Digest = result.ManifestDigest
	commit.Status.Images = result.Images
	commit.Status.LayerDiffID = result.DiffID
	commit.Status.LayerSize = result.LayerSize
	commit.Status.BaseImageDigest = result.BaseDigest
	commit.Status.CompletionTime = &now
	commit.Status.Message = fmt.Sprintf("Pushed %s", result.Images[0])
	if commit.Spec.RebaseDevBox {
		commit.Status.Message += r.scheduleRebase(ctx, commit, result.Images[0])
	}
	r.setCommitCondition(commit, metav1.ConditionTrue, "Pushed", commit.Status.Message)
	logf.FromContext(ctx).Info("Commit succeeded",
		"commit", commit.Name, "digest", result.ManifestDigest, "images", result.Images)
	return nil
}

// scheduleRebase points the DevBox at the image this commit published, so its
// next start switches base image and clears the previous upper layer.
// resolveSource already keeps a live commit away from rebaseDevBox; the pod is
// checked again because one can appear while the build runs, and clearing
// upper/ afterwards would drop every write it made since the snapshot.
//
// The returned suffix reaches the commit message: the image is published either
// way, so a rebase that could not be scheduled is reported there rather than
// turning a successful push into a failed commit.
func (r *DevBoxCommitReconciler) scheduleRebase(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, image string,
) string {
	log := logf.FromContext(ctx)

	box := &devpilotv1alpha1.DevBox{}
	key := types.NamespacedName{Namespace: commit.Namespace, Name: commit.Spec.DevBoxName}
	if err := r.Get(ctx, key, box); err != nil {
		log.Error(err, "Could not read the DevBox to schedule its rebase", "devbox", commit.Spec.DevBoxName)
		return "; could not schedule the rebase on " + commit.Spec.DevBoxName
	}

	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: box.Namespace, Name: box.Name + PodSuffix}
	switch err := r.Get(ctx, podKey, pod); {
	case err == nil:
		log.Info("Skipped rebase scheduling: the root disk is in use", "devbox", box.Name, "pod", pod.Name)
		return fmt.Sprintf("; %s started before the build finished, so pendingRebaseImage was not set", box.Name)
	case !isNotFound(err):
		log.Error(err, "Could not read the DevBox pod to schedule its rebase", "devbox", box.Name)
		return "; could not schedule the rebase on " + box.Name
	}

	box.Status.PendingRebaseImage = image
	if err := r.Status().Update(ctx, box); err != nil {
		log.Error(err, "Could not schedule the DevBox rebase", "devbox", box.Name)
		return "; could not schedule the rebase on " + box.Name
	}
	log.Info("Scheduled the DevBox rebase", "devbox", box.Name, "image", image)
	return fmt.Sprintf("; %s will rebase onto it on its next start", box.Name)
}

// terminationMessage returns the message the commit binary wrote. The kubelet
// copies the file into the container state, so no second log stream is needed.
func (r *DevBoxCommitReconciler) terminationMessage(ctx context.Context, job *batchv1.Job) string {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
		logf.FromContext(ctx).Error(err, "Could not list the build Job pods", "job", job.Name)
		return ""
	}
	for i := range pods.Items {
		for _, status := range pods.Items[i].Status.ContainerStatuses {
			if status.Name != CommitContainerName {
				continue
			}
			if state := status.State.Terminated; state != nil && state.Message != "" {
				return state.Message
			}
			if state := status.LastTerminationState.Terminated; state != nil && state.Message != "" {
				return state.Message
			}
		}
	}
	return ""
}

// commitInProgress reports whether an earlier commit of the same DevBox still
// holds the lock. Two builds reading one root disk would race on the same PVC.
func (r *DevBoxCommitReconciler) commitInProgress(ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit) (bool, error) {
	commits := &devpilotv1alpha1.DevBoxCommitList{}
	if err := r.List(ctx, commits, client.InNamespace(commit.Namespace)); err != nil {
		return false, fmt.Errorf("list devboxcommits: %w", err)
	}
	for i := range commits.Items {
		other := &commits.Items[i]
		if other.Name == commit.Name || other.Spec.DevBoxName != commit.Spec.DevBoxName || !other.Active() {
			continue
		}
		// The oldest active commit wins; the rest wait their turn.
		if other.CreationTimestamp.Before(&commit.CreationTimestamp) ||
			(other.CreationTimestamp.Equal(&commit.CreationTimestamp) && other.Name < commit.Name) {
			return true, nil
		}
	}
	return false, nil
}

// failCommit lands the commit in Failed with a condition reason the user can
// act on. The error is returned so a transient cause is retried, unless it is
// a permanent configuration or input problem.
func (r *DevBoxCommitReconciler) failCommit(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, reason string, err error,
) error {
	r.releaseEphemeralBestEffort(ctx, commit)

	now := nowUTC()
	commit.Status.Phase = devpilotv1alpha1.CommitPhaseFailed
	commit.Status.Message = err.Error()
	commit.Status.CompletionTime = &now
	r.setCommitCondition(commit, metav1.ConditionFalse, reason, err.Error())
	logf.FromContext(ctx).Error(err, "DevBoxCommit failed", "commit", commit.Name, "reason", reason)
	return nil
}

// setCommitCondition upserts the Ready condition, keeping the transition time
// stable while the observed state does not change.
func (r *DevBoxCommitReconciler) setCommitCondition(
	commit *devpilotv1alpha1.DevBoxCommit, status metav1.ConditionStatus, reason, message string,
) {
	now := metav1.NewTime(timeNow())
	for i := range commit.Status.Conditions {
		cond := &commit.Status.Conditions[i]
		if cond.Type != CommitConditionReady {
			continue
		}
		if cond.Status != status {
			cond.LastTransitionTime = now
		}
		cond.Status = status
		cond.Reason = reason
		cond.Message = message
		cond.ObservedGeneration = commit.Generation
		return
	}
	commit.Status.Conditions = append(commit.Status.Conditions, metav1.Condition{
		Type:               CommitConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: commit.Generation,
		LastTransitionTime: now,
	})
}

// patchCommitStatus writes the status subresource against the pre-reconcile
// status so a concurrent writer's changes survive.
func (r *DevBoxCommitReconciler) patchCommitStatus(
	ctx context.Context, before, after *devpilotv1alpha1.DevBoxCommit,
) error {
	after.Status.ObservedGeneration = after.Generation
	base := after.DeepCopy()
	base.Status = before.Status
	if err := r.Status().Patch(ctx, after, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch DevBoxCommit status: %w", err)
	}
	return nil
}

// finalize releases the build Job. The Job carries an owner reference, but an
// explicit delete makes the release prompt instead of waiting for the garbage
// collector, and it also covers a Job orphaned by an interrupted create.
func (r *DevBoxCommitReconciler) finalize(ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit) error {
	if !controllerutil.ContainsFinalizer(commit, CommitFinalizer) {
		return nil
	}
	if err := r.releaseBuildJob(ctx, commit); err != nil {
		return err
	}
	// A commit deleted mid-flight still holds a clone claim and a snapshot.
	// Releasing them is part of the finalizer contract, so an error here keeps
	// the commit around for a retry instead of leaking the storage.
	if err := r.releaseEphemeral(ctx, commit); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(commit, CommitFinalizer)
	if err := r.Update(ctx, commit); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove commit finalizer: %w", err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DevBoxCommitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devpilotv1alpha1.DevBoxCommit{}).
		Owns(&batchv1.Job{}).
		// The snapshot becomes ready asynchronously, so its status change is the
		// signal that a restore can start instead of the poll interval.
		Owns(&snapshotv1.VolumeSnapshot{}).
		Named("devboxcommit").
		Complete(r)
}

// buildJobName derives the Job name from the commit, which is unique per
// namespace, so a commit never adopts another commit's build.
func buildJobName(commit *devpilotv1alpha1.DevBoxCommit) string {
	return commit.Name + CommitJobSuffix
}

// envPairs renders the config env overlay as the CLI's NAME=VALUE list.
func envPairs(env []devpilotv1alpha1.NameValue) string {
	pairs := make([]string, 0, len(env))
	for _, entry := range env {
		pairs = append(pairs, entry.Name+"="+entry.Value)
	}
	return strings.Join(pairs, ",")
}
