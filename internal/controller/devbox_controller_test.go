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

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

// fakeResolver returns a fixed digest and command without touching a registry.
type fakeResolver struct {
	info ImageInfo
	err  error
	// digestByRef overrides info.Digest per image reference, so a test can
	// simulate a tag that moved.
	digestByRef map[string]string
	calls       int
}

func (f *fakeResolver) Resolve(_ context.Context, image string) (ImageInfo, error) {
	f.calls++
	if f.err != nil {
		return ImageInfo{}, f.err
	}
	info := f.info
	if digest, ok := f.digestByRef[image]; ok {
		info.Digest = digest
	}
	info.ImageRef = image
	return info, nil
}

const (
	testDigest = "sha256:0b2f5d3a8db1c8e1a3e25e3a2f4c3b2a1f0e9d8c7b6a5948372615041320ffee"
	testInit   = "registry.example.com/devpilot-init:test"
)

func newTestResolver() *fakeResolver {
	return &fakeResolver{info: ImageInfo{
		Digest:     testDigest,
		Entrypoint: []string{"/opt/nvidia/nvidia_entrypoint.sh"},
		Cmd:        []string{"python", "-m", "torch.distributed.run"},
	}}
}

// newTestDevBox returns a spec that the reconciler can act on without a real
// registry or storage provisioner.
func newTestDevBox(name, namespace string) *devpilotv1alpha1.DevBox {
	return &devpilotv1alpha1.DevBox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: devpilotv1alpha1.DevBoxSpec{
			Image: testDevBoxImage,
			Resources: devpilotv1alpha1.DevBoxResources{
				GPUs:     1,
				GPUModel: "a100",
			},
			RootDisk: devpilotv1alpha1.RootDiskSpec{
				Size:             resource.MustParse(testVolumeSize),
				StorageClassName: testStorageClassNVMe,
			},
		},
	}
}

func newTestReconciler(resolver ImageResolver) *DevBoxReconciler {
	return &DevBoxReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		Config: Config{InitImage: testInit},
		Images: resolver,
	}
}

func reconcileOnce(r *DevBoxReconciler, key types.NamespacedName) (reconcile.Result, error) {
	return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
}

// markClaimBound simulates the storage provisioner, which envtest does not run.
// A volume name is required too: the API server only accepts an expansion on a
// dynamically provisioned claim.
func markClaimBound(key types.NamespacedName) {
	pvc := &corev1.PersistentVolumeClaim{}
	Expect(k8sClient.Get(ctx, key, pvc)).To(Succeed())
	pvc.Spec.VolumeName = "pv-" + pvc.Name
	Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
	pvc.Status.Phase = corev1.ClaimBound
	Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
}

var specCounter int

var _ = Describe("DevBox Controller", func() {
	var (
		namespace string
		resolver  *fakeResolver
		r         *DevBoxReconciler
	)

	BeforeEach(func() {
		// Every spec gets its own namespace: deleting one leaves it terminating
		// for a while, and a reused name would reject new objects.
		specCounter++
		namespace = fmt.Sprintf("devbox-%d-%d", GinkgoRandomSeed(), specCounter)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		resolver = newTestResolver()
		r = newTestReconciler(resolver)
	})

	BeforeEach(func() {
		// The API server rejects pods naming an absent RuntimeClass; the GPU
		// nodes on bz1-dev1 provide the real one through the NVIDIA operator.
		runtimeClass := &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "nvidia"},
			Handler:    "nvidia",
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, runtimeClass))).To(Succeed())

		// Envtest ships no storage class, and the API server only allows a
		// claim to grow when its class opts into expansion.
		expand := true
		storageClass := &storagev1.StorageClass{
			ObjectMeta:           metav1.ObjectMeta{Name: testStorageClassNVMe},
			Provisioner:          "example.com/fake",
			AllowVolumeExpansion: &expand,
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, storageClass))).To(Succeed())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	})

	create := func(box *devpilotv1alpha1.DevBox) types.NamespacedName {
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		return client.ObjectKeyFromObject(box)
	}

	It("reports a missing DevBox as a no-op", func() {
		key := types.NamespacedName{Namespace: namespace, Name: "absent"}
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
	})

	It("creates the root disk claim before the pod", func() {
		box := newTestDevBox("first-start", namespace)
		key := create(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		By("creating a claim owned by the DevBox")
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + RootDiskPVCSuffix,
		}, pvc)).To(Succeed())
		Expect(*pvc.Spec.StorageClassName).To(Equal(testStorageClassNVMe))
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal(testVolumeSize))
		Expect(pvc.OwnerReferences).To(HaveLen(1))
		Expect(pvc.OwnerReferences[0].Name).To(Equal(box.Name))

		By("waiting for the claim instead of starting the pod")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhasePending))
		Expect(box.Status.StartingStep).To(Equal(devpilotv1alpha1.StartingStepAllocatingResources))
		Expect(box.Status.RootDiskPVC).To(Equal(box.Name + RootDiskPVCSuffix))

		pod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)
		Expect(err).To(HaveOccurred())
	})

	It("starts the pod once the claim is bound", func() {
		box := newTestDevBox("bound", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())
		Expect(pod.Labels[LabelDevBoxName]).To(Equal(box.Name))
		Expect(pod.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(pod.Spec.InitContainers).To(HaveLen(1))
		Expect(pod.Spec.InitContainers[0].Image).To(Equal(testInit))
		Expect(pod.Spec.InitContainers[0].Args).To(ContainElement("--install=" + InitBinaryPath))
		Expect(pod.Spec.Containers).To(HaveLen(1))

		main := pod.Spec.Containers[0]
		Expect(main.Command[0]).To(Equal(InitBinaryPath))
		Expect(main.Command).To(ContainElement("--image-digest=" + testDigest))
		Expect(main.Command[len(main.Command)-1]).To(Equal("torch.distributed.run"))
		Expect(*pod.Spec.RuntimeClassName).To(Equal("nvidia"))
		Expect(pod.Spec.NodeSelector).To(HaveKeyWithValue(DefaultGPUNodeLabel, "a100"))

		By("recording the pinned digest")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.ActiveImageDigest).To(Equal(testDigest))
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStarting))
	})

	// pivot_root is denied with EPERM under RuntimeDefault: the profile only
	// allows it for CAP_SYS_ADMIN holders while no_new_privs is off, and
	// allowPrivilegeEscalation=false sets no_new_privs. Losing that assertion
	// would only surface on a real cluster, as a DevBox stuck in Starting.
	It("lifts the seccomp profile pod-wide and keeps capabilities minimal", func() {
		box := newTestDevBox("seccomp", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())

		By("setting the profile at pod level")
		Expect(pod.Spec.SecurityContext).NotTo(BeNil())
		Expect(pod.Spec.SecurityContext.SeccompProfile).NotTo(BeNil())
		Expect(pod.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeUnconfined))

		By("never overriding it per container")
		for _, container := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
			if container.SecurityContext != nil {
				Expect(container.SecurityContext.SeccompProfile).To(BeNil())
			}
		}

		By("keeping the compensating capability restrictions")
		Expect(*pod.Spec.AutomountServiceAccountToken).To(BeFalse())
		main := pod.Spec.Containers[0]
		Expect(main.SecurityContext).NotTo(BeNil())
		Expect(main.SecurityContext.Privileged).To(BeNil())
		Expect(*main.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(main.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
		// SYS_ADMIN is only needed while the overlay is mounted; devpilot-init
		// drops it from the bounding set once the root disk is in place.
		Expect(main.SecurityContext.Capabilities.Add).To(ConsistOf(
			corev1.Capability("CHOWN"), corev1.Capability("DAC_OVERRIDE"),
			corev1.Capability("FOWNER"), corev1.Capability("FSETID"),
			corev1.Capability("KILL"), corev1.Capability("MKNOD"),
			corev1.Capability("NET_BIND_SERVICE"), corev1.Capability("SETGID"),
			corev1.Capability("SETUID"), corev1.Capability("SYS_ADMIN"),
			corev1.Capability("SYS_CHROOT"),
		))
	})

	It("refuses to mount the root disk over a base image that moved", func() {
		box := newTestDevBox("moved", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		resolver.digestByRef = map[string]string{box.Spec.Image: "sha256:deadbeef"}

		_, err = reconcileOnce(r, key)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("commit it or reset the root disk"))

		By("failing the DevBox with an actionable message")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseFailed))
		Expect(box.Status.Message).To(ContainSubstring("commit it or reset the root disk"))
	})

	It("reports Running when the pod is ready", func() {
		box := newTestDevBox("running", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())
		// nodeName is set by the scheduler, which envtest does not run, and the
		// API server forbids clients from setting it. Only the pod status is
		// simulated here; the node name is covered by the bz1-dev1 run.
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
		Expect(box.Status.PodName).To(Equal(box.Name + PodSuffix))
		Expect(metaConditionStatus(box, ConditionReady)).To(Equal(metav1.ConditionTrue))
	})

	It("stops the DevBox and keeps the root disk", func() {
		box := newTestDevBox("stopping", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopping))

		By("landing in Stopped once the pod is gone")
		podKey := types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}
		err = k8sClient.Get(ctx, podKey, &corev1.Pod{})
		if err == nil {
			// envtest does not run kubelet or the garbage collector's pod
			// controller, so remove the pod the way the API server would.
			Expect(k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: podKey.Name, Namespace: podKey.Namespace,
			}})).To(Succeed())
		}
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopped))
		Expect(box.Status.StoppedAt).NotTo(BeNil())
		Expect(box.Status.NodeName).To(BeEmpty())
		Expect(box.Status.PodName).To(BeEmpty())

		By("keeping the claim")
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + RootDiskPVCSuffix,
		}, &corev1.PersistentVolumeClaim{})).To(Succeed())
	})

	It("applies the stop annotation exactly once", func() {
		box := newTestDevBox("action-stop", namespace)
		box.Annotations = map[string]string{AnnotationAction: "stop=" + epochNow()}
		key := create(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateStopped))
		Expect(box.Annotations[AnnotationActionHandled]).To(Equal(box.Annotations[AnnotationAction]))

		By("not restarting work when the annotation is replayed")
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateRunning))
	})

	It("rejects an unknown action", func() {
		box := newTestDevBox("action-bad", namespace)
		box.Annotations = map[string]string{AnnotationAction: "explode"}
		key := create(box)

		// The error is permanent until the user fixes the annotation, so it is
		// reported in status instead of being retried with backoff.
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseFailed))
		Expect(box.Status.Message).To(ContainSubstring("unsupported action"))
	})

	It("refuses reset-rootdisk while the DevBox is running", func() {
		box := newTestDevBox("action-reset-running", namespace)
		box.Annotations = map[string]string{AnnotationAction: "reset-rootdisk"}
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		// Force a running phase, then replay the action.
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Status.Phase = devpilotv1alpha1.PhaseRunning
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())
		delete(box.Annotations, AnnotationActionHandled)
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseFailed))
		Expect(box.Status.Message).To(ContainSubstring("reset-rootdisk requires a stopped devbox"))
	})

	It("resets the root disk from a stopped DevBox", func() {
		box := newTestDevBox("action-reset", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pvcKey := types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix}
		Expect(k8sClient.Get(ctx, pvcKey, &corev1.PersistentVolumeClaim{})).To(Succeed())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Status.Phase = devpilotv1alpha1.PhaseStopped
		box.Status.ActiveImageDigest = testDigest
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())
		// A status update returns the object without the annotations that
		// were only set locally, so they must be applied after it.
		box.Annotations = map[string]string{AnnotationAction: "reset-rootdisk=" + epochNow()}
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.ActiveImageDigest).To(BeEmpty())
		Expect(box.Status.RootDiskPVC).To(BeEmpty())

		err = k8sClient.Get(ctx, pvcKey, &corev1.PersistentVolumeClaim{})
		Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
	})

	It("replaces the pod when the spec changes", func() {
		box := newTestDevBox("respec", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		podKey := types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}
		before := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey, before)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Spec.Resources.CPU = resource.MustParse("8")
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStarting))

		after := &corev1.Pod{}
		err = k8sClient.Get(ctx, podKey, after)
		Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		if err == nil {
			Expect(podHash(after)).NotTo(Equal(podHash(before)))
		}
	})

	It("grows the root disk in place", func() {
		box := newTestDevBox("grow", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		// The API server only allows expanding a bound claim's requests.
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Spec.RootDisk.Size = resource.MustParse("80Gi")
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + RootDiskPVCSuffix,
		}, pvc)).To(Succeed())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("80Gi"))
	})

	It("surfaces a failed pod as Failed", func() {
		box := newTestDevBox("failed", namespace)
		key := create(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Message = "devpilot-init: base image digest mismatch"
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseFailed))
		Expect(box.Status.Message).To(ContainSubstring("digest mismatch"))
	})
})

// metaConditionStatus reads one condition's status, failing if it is absent.
func metaConditionStatus(box *devpilotv1alpha1.DevBox, condType string) metav1.ConditionStatus {
	for _, cond := range box.Status.Conditions {
		if cond.Type == condType {
			return cond.Status
		}
	}
	return ""
}
