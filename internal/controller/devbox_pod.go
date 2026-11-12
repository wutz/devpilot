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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

// applyAction consumes the one-shot action annotation. It reports whether it
// changed the object, which tells Reconcile to persist status and stop.
func (r *DevBoxReconciler) applyAction(ctx context.Context, box *devpilotv1alpha1.DevBox) (bool, error) {
	log := logf.FromContext(ctx)

	raw := strings.TrimSpace(box.Annotations[AnnotationAction])
	if raw == "" {
		return false, nil
	}
	parsed, err := parseAction(raw)
	if err != nil {
		return false, err
	}

	// The annotation is consumed by deleting it in the same update that carries
	// the spec change. A marker was tried first and broke the second request
	// with the same verb: "stop" is the same string every time, so the marker
	// made a later stop look like a replay of the first one.
	delete(box.Annotations, AnnotationAction)

	switch parsed.verb {
	case actionStop:
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
	case actionStart:
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
		r.resumeInterrupted(box)
	case actionRestart:
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
		r.resumeInterrupted(box)
		if err := r.deletePod(ctx, box.Status.PodName, box.Namespace); err != nil {
			return false, err
		}
		log.Info("Restarting DevBox", "pod", box.Status.PodName)
	case actionResetRootDisk:
		if !canReset(box.Status.Phase) {
			return false, fmt.Errorf("reset-rootdisk requires a stopped devbox, current phase is %q", box.Status.Phase)
		}
		if err := r.deletePod(ctx, box.Status.PodName, box.Namespace); err != nil {
			return false, err
		}
		if err := r.deleteRootDisk(ctx, box); err != nil {
			return false, err
		}
		log.Info("Reset DevBox root disk", "devbox", box.Name)
	}

	// The spec change and the annotation removal go out together, so a crash
	// between them can never replay the action against the new spec. The
	// update response carries the server's status, so status mutations are
	// re-applied afterwards and persisted by Reconcile's status patch.
	status := box.Status
	if err := r.Update(ctx, box); err != nil {
		return false, fmt.Errorf("apply action %q: %w", raw, err)
	}
	box.Status = status

	switch parsed.verb {
	case actionRestart:
		box.Status.Message = "Restarting the devbox pod"
	case actionResetRootDisk:
		box.Status.ActiveImage = ""
		box.Status.ActiveImageDigest = ""
		box.Status.RootDiskPVC = ""
		box.Status.Phase = devpilotv1alpha1.PhasePending
		box.Status.Message = "Root disk deleted; it is recreated on the next start"
	}
	return true, nil
}

// deletePod removes the named pod, tolerating absence.
func (r *DevBoxReconciler) deletePod(ctx context.Context, name, namespace string) error {
	if name == "" {
		return nil
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	err := r.Delete(ctx, pod, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}
	return nil
}

// deleteRootDisk removes the root disk claim; the next start recreates it from
// the pinned base image.
func (r *DevBoxReconciler) deleteRootDisk(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	name := box.Status.RootDiskPVC
	if name == "" {
		name = box.Name + RootDiskPVCSuffix
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: box.Namespace},
	}
	if err := r.Delete(ctx, pvc); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete root disk %s: %w", name, err)
	}
	return nil
}

// ensureRootDisk creates the claim when missing and grows it in place when the
// user raises spec.rootDisk.size.
func (r *DevBoxReconciler) ensureRootDisk(ctx context.Context, box *devpilotv1alpha1.DevBox) (*corev1.PersistentVolumeClaim, error) {
	name := box.Name + RootDiskPVCSuffix
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: name}, pvc)
	if isNotFound(err) {
		wanted, buildErr := r.rootDiskPVC(box, name)
		if buildErr != nil {
			return nil, buildErr
		}
		logf.FromContext(ctx).Info("Creating root disk claim", "pvc", name, "size", box.Spec.RootDisk.Size.String())
		if createErr := r.Create(ctx, wanted); createErr != nil {
			if isAlreadyExists(createErr) {
				// Another reconcile won the race; read the winner back.
				if getErr := r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: name}, pvc); getErr != nil {
					return nil, fmt.Errorf("get root disk %s: %w", name, getErr)
				}
			} else {
				return nil, fmt.Errorf("create root disk %s: %w", name, createErr)
			}
		} else {
			return wanted, nil
		}
	} else if err != nil {
		return nil, fmt.Errorf("get root disk %s: %w", name, err)
	}

	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != box.Spec.RootDisk.StorageClassName {
		return nil, fmt.Errorf("root disk %s uses storage class %q but the spec requires %q; "+
			"reset the root disk to move it", name, *pvc.Spec.StorageClassName, box.Spec.RootDisk.StorageClassName)
	}
	if err := r.growRootDisk(ctx, pvc, box.Spec.RootDisk.Size); err != nil {
		return nil, err
	}
	return pvc, nil
}

// growRootDisk raises the requested size. Kubernetes never shrinks a claim, so
// a smaller spec is ignored rather than rejected at reconcile time.
func (r *DevBoxReconciler) growRootDisk(ctx context.Context, pvc *corev1.PersistentVolumeClaim, wanted apiresource.Quantity) error {
	if wanted.Cmp(pvc.Spec.Resources.Requests[corev1.ResourceStorage]) <= 0 {
		return nil
	}

	key := client.ObjectKeyFromObject(pvc)
	for range 3 {
		latest := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("get root disk %s for expansion: %w", key.Name, err)
		}
		if wanted.Cmp(latest.Spec.Resources.Requests[corev1.ResourceStorage]) <= 0 {
			return nil
		}
		requests := latest.Spec.Resources.Requests.DeepCopy()
		if requests == nil {
			requests = corev1.ResourceList{}
		}
		requests[corev1.ResourceStorage] = wanted
		latest.Spec.Resources.Requests = requests
		if err := r.Update(ctx, latest); err != nil {
			if isConflict(err) {
				continue
			}
			return fmt.Errorf("expand root disk %s: %w", key.Name, err)
		}
		logf.FromContext(ctx).Info("Expanded root disk claim", "pvc", key.Name, "size", wanted.String())
		return nil
	}
	return fmt.Errorf("expand root disk %s: too many update conflicts", key.Name)
}

// rootDiskPVC builds the claim for a new DevBox.
func (r *DevBoxReconciler) rootDiskPVC(box *devpilotv1alpha1.DevBox, name string) (*corev1.PersistentVolumeClaim, error) {
	storageClass := box.Spec.RootDisk.StorageClassName
	mode := corev1.PersistentVolumeFilesystem
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: box.Namespace,
			Labels: map[string]string{
				LabelManagedBy:  ValueManagedBy,
				LabelDevBoxName: box.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			VolumeMode:       &mode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: box.Spec.RootDisk.Size,
				},
			},
		},
	}
	if err := ctrlSetOwner(pvc, box, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on root disk claim: %w", err)
	}
	return pvc, nil
}

// buildPod assembles the pod template. It is a pure function of the spec, the
// resolved image and the claim name, so the spec hash stays stable.
// buildPod assembles the pod template. It is a pure function of the spec, the
// resolved image, the claim name and the rebase decision, so the spec hash
// stays stable. rebase is left out of the hash on purpose: once the rebase is
// consumed it turns off, and a flag that only matters at start must not make
// the running pod look outdated.
func (r *DevBoxReconciler) buildPod(box *devpilotv1alpha1.DevBox, pvc *corev1.PersistentVolumeClaim, info ImageInfo, rebase bool) (*corev1.Pod, error) {
	limits, requests := containerResources(box.Spec.Resources)
	shm, err := shmSize(box.Spec.Resources)
	if err != nil {
		return nil, err
	}
	hash, err := specHash(box, pvc.Name, info)
	if err != nil {
		return nil, err
	}

	env := append([]corev1.EnvVar{
		{Name: "DEVPILOT_BASE_IMAGE", Value: info.ImageRef},
		{Name: "DEVPILOT_BASE_IMAGE_DIGEST", Value: info.Digest},
		{Name: "DEVPILOT_ROOT_DISK", Value: RootDiskMountPath},
		{Name: "HOME", Value: "/root"},
	}, box.Spec.Env...)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      box.Name + PodSuffix,
			Namespace: box.Namespace,
			Labels: map[string]string{
				LabelManagedBy:  ValueManagedBy,
				LabelDevBoxName: box.Name,
			},
			Annotations: map[string]string{
				AnnotationSpecHash: hash,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			// RuntimeDefault rejects pivot_root with EPERM: the profile only
			// allows it for CAP_SYS_ADMIN holders when no_new_privs is off, and
			// allowPrivilegeEscalation=false sets no_new_privs. Both devpilot-init
			// and the user entrypoint share this pod, so the profile is lifted
			// pod-wide; the containers stay non-privileged, keep ALL capabilities
			// dropped, and devpilot-init narrows to its explicit add list.
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			},
			InitContainers: []corev1.Container{initContainer(r.Config.InitImage)},
			Containers:     []corev1.Container{mainContainer(box, info, rebase, env, limits, requests, shm)},
			Volumes:        volumes(box, pvc, shm),
			NodeSelector:   nodeSelectors(box),
			Tolerations:    box.Spec.Tolerations,
		},
	}
	if box.Spec.Resources.GPUs > 0 {
		pod.Spec.RuntimeClassName = new("nvidia")
	}
	if err := ctrlSetOwner(pod, box, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on pod: %w", err)
	}
	return pod, nil
}

// initContainer copies the helper binary into the shared emptyDir. The user
// image needs neither a shell nor cp, so arbitrary distroless images work.
func initContainer(image string) corev1.Container {
	return corev1.Container{
		Name:            InitContainerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/devpilot-init"},
		Args:            []string{"--install=" + InitBinaryPath},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(0)),
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{capabilityAll},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: InitVolumeName, MountPath: InitMountPath},
		},
	}
}

// mainContainer runs devpilot-init as PID 1, which assembles the overlay and
// supervises the image's own entrypoint.
func mainContainer(box *devpilotv1alpha1.DevBox, info ImageInfo, rebase bool, env []corev1.EnvVar, limits, requests corev1.ResourceList, shm *apiresource.Quantity) corev1.Container {
	_ = shm // reserved for a future explicit tmpfs mount flag
	return corev1.Container{
		Name:            MainContainerName,
		Image:           info.ImageRef,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         append([]string{InitBinaryPath}, devBoxCommand(box, info, rebase)...),
		Env:             env,
		Resources: corev1.ResourceRequirements{
			Limits:   limits,
			Requests: requests,
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(0)),
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(false),
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{
					"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
					"NET_BIND_SERVICE", "SETGID", "SETUID", "SYS_ADMIN", "SYS_CHROOT",
				},
				Drop: []corev1.Capability{capabilityAll},
			},
		},
		// Advertise the access ports so `kubectl describe pod` and any tooling
		// reading the pod spec see what the devbox serves, matching the Service.
		Ports:        containerPorts(box),
		VolumeMounts: volumeMounts(box),
	}
}

// containerPorts lists the ports the container listens on for each access path.
func containerPorts(box *devpilotv1alpha1.DevBox) []corev1.ContainerPort {
	ports := make([]corev1.ContainerPort, 0, 2+len(box.Spec.Access.Ports))
	if sshEnabled(box) {
		ports = append(ports, containerPort(SSHServicePortName, SSHPort))
	}
	if jupyterEnabled(box) {
		ports = append(ports, containerPort(JupyterServicePortName, jupyterPort(box)))
	}
	for _, extra := range box.Spec.Access.Ports {
		ports = append(ports, containerPort(extra.Name, extra.Port))
	}
	return ports
}

// containerPort renders one container port with the shared TCP protocol.
func containerPort(name string, port int32) corev1.ContainerPort {
	return corev1.ContainerPort{Name: name, ContainerPort: port, Protocol: corev1.ProtocolTCP}
}

// volumeMounts lists the main container's mounts. The SSH keys are mounted
// read-only: the host key is operator state, not a file a session may rewrite.
func volumeMounts(box *devpilotv1alpha1.DevBox) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: InitVolumeName, MountPath: InitMountPath},
		{Name: RootDiskVolumeName, MountPath: RootDiskMountPath},
		{Name: ShmVolumeName, MountPath: "/dev/shm"},
	}
	if sshEnabled(box) {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      SSHVolumeName,
			MountPath: SSHMountPath,
			ReadOnly:  true,
		})
	}
	if jupyterEnabled(box) {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      JupyterVolumeName,
			MountPath: JupyterMountPath,
			ReadOnly:  true,
		})
	}
	return mounts
}

// shellNames are the interpreters that exit immediately without a TTY: with
// stdin on /dev/null the shell reads EOF and quits, which turns a pod meant to
// host an interactive session into a Finished one.
var shellNames = map[string]struct{}{
	"bash": {}, "dash": {}, "ash": {}, "sh": {}, "zsh": {}, "fish": {},
}

// startCommand resolves the process devpilot-init should supervise. An explicit
// spec.command wins; otherwise the image's own ENTRYPOINT/CMD is inherited,
// unless that only opens a shell or the image declares no CMD at all. A
// CMD-less image has no command of its own: nvcr.io/nvidia/pytorch sets
// ENTRYPOINT=/opt/nvidia/nvidia_entrypoint.sh with Cmd=None, and that wrapper
// prints its banner and exec's an empty argument list, so the pod would exit at
// once. An image whose ENTRYPOINT alone is the server needs spec.command.
// Flags must precede "--"; everything after it is exec'd as the user process.
func startCommand(box *devpilotv1alpha1.DevBox, info ImageInfo) []string {
	if len(box.Spec.Command) > 0 {
		return append(append([]string{}, box.Spec.Command...), box.Spec.Args...)
	}
	argv := append(append([]string{}, info.Entrypoint...), info.Cmd...)
	if len(info.Cmd) == 0 || isBareShell(argv) {
		// Keep the container alive so the user can attach, as documented. A
		// shell is reachable through the session, not through PID 1's stdin.
		return []string{"sleep", "infinity"}
	}
	return argv
}

// isBareShell reports whether the resolved command ends in an interpreter, with
// no script or flag that would keep it busy. An image entrypoint that only
// exec's "$@" (the NVIDIA images do) leaves the CMD's shell as the real process,
// so the last argument decides.
func isBareShell(argv []string) bool {
	last := argv[len(argv)-1]
	_, bare := shellNames[last[strings.LastIndex(last, "/")+1:]]
	return bare
}

// devBoxCommand renders the flags and the user command for devpilot-init.
// --rebase clears upper/ before the overlay is assembled, so it must be a flag
// rather than anything after "--", which is exec'd as the user process.
func devBoxCommand(box *devpilotv1alpha1.DevBox, info ImageInfo, rebase bool) []string {
	user := startCommand(box, info)
	argv := make([]string, 0, 8+len(user))
	argv = append(argv, "--image-ref="+info.ImageRef, "--image-digest="+info.Digest)
	if rebase {
		argv = append(argv, "--rebase")
	}
	if sshEnabled(box) {
		argv = append(argv, sshCommandFlags()...)
	}
	if jupyterEnabled(box) {
		argv = append(argv, jupyterCommandFlags(box)...)
	}
	argv = append(argv, "--")
	return append(argv, user...)
}

// volumes lists the pod volumes: the helper binary, the root disk and shared
// memory. /dev/shm becomes a memory backed emptyDir so the default 64Mi limit
// does not break DataLoader workers.
func volumes(box *devpilotv1alpha1.DevBox, pvc *corev1.PersistentVolumeClaim, shm *apiresource.Quantity) []corev1.Volume {
	vols := []corev1.Volume{
		{
			Name:         InitVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: RootDiskVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name},
			},
		},
		{
			Name: ShmVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: shm,
				},
			},
		},
	}
	if sshEnabled(box) {
		vols = append(vols, sshVolume(box))
	}
	if jupyterEnabled(box) {
		vols = append(vols, jupyterVolume(box))
	}
	seen := map[string]struct{}{
		InitVolumeName:     {},
		RootDiskVolumeName: {},
		ShmVolumeName:      {},
		SSHVolumeName:      {},
		JupyterVolumeName:  {},
	}
	for _, extra := range box.Spec.Volumes {
		if _, dup := seen[extra.Name]; dup || extra.PersistentVolumeClaim == nil {
			continue
		}
		seen[extra.Name] = struct{}{}
		vols = append(vols, corev1.Volume{
			Name: extra.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: extra.PersistentVolumeClaim.DeepCopy(),
			},
		})
	}
	return vols
}

// setFailed marks the DevBox as failed without blocking future reconciles: a
// spec change or a new action moves it forward again.
func (r *DevBoxReconciler) setFailed(ctx context.Context, box *devpilotv1alpha1.DevBox, reason string, err error) {
	box.Status.Phase = devpilotv1alpha1.PhaseFailed
	box.Status.StartingStep = devpilotv1alpha1.StartingStepNone
	box.Status.Message = err.Error()
	r.setCondition(box, ConditionReady, metav1.ConditionFalse, reason, err.Error())
	logf.FromContext(ctx).Error(err, "DevBox failed", "devbox", box.Name, "reason", reason)
}

// setCondition upserts one condition, keeping the transition time stable while
// the observed state does not change.
func (r *DevBoxReconciler) setCondition(box *devpilotv1alpha1.DevBox, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.NewTime(timeNow())
	for i := range box.Status.Conditions {
		cond := &box.Status.Conditions[i]
		if cond.Type != condType {
			continue
		}
		if cond.Status != status {
			cond.LastTransitionTime = now
		}
		cond.Status = status
		cond.Reason = reason
		cond.Message = message
		cond.ObservedGeneration = box.Generation
		return
	}
	box.Status.Conditions = append(box.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: box.Generation,
		LastTransitionTime: now,
	})
}

// clearCondition drops a condition that no longer applies, so a spec that turns
// a feature off does not leave a stale verdict behind.
func (r *DevBoxReconciler) clearCondition(box *devpilotv1alpha1.DevBox, condType string) {
	kept := box.Status.Conditions[:0]
	for _, cond := range box.Status.Conditions {
		if cond.Type != condType {
			kept = append(kept, cond)
		}
	}
	box.Status.Conditions = kept
}

// patchStatus writes the status subresource, merging from the pre-reconcile
// status so a concurrent writer's changes are not clobbered.
func (r *DevBoxReconciler) patchStatus(ctx context.Context, before, after *devpilotv1alpha1.DevBox) error {
	after.Status.ObservedGeneration = after.Generation
	base := after.DeepCopy()
	base.Status = before.Status
	if err := r.Status().Patch(ctx, after, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch DevBox status: %w", err)
	}
	return nil
}
