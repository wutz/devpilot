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
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	scenarioImageV2  = "nvcr.io/nvidia/pytorch:25.08-py3"
	scenarioDigestV2 = "sha256:22223333444455556666777788889999aaaabbbbccccddddeeeeffff00001111"
	scenarioZone     = "cn-east-1a"
	scenarioZoneOvrd = "cn-east-1b"
)

var _ = Describe("User Perspective Scenarios", func() {
	var (
		namespace string
		resolver  *fakeResolver
		r         *DevBoxReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("scenario-%d-%d", GinkgoRandomSeed(), specCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
		resolver = newTestResolver()
		r = newTestReconciler(resolver)
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	})

	markPodReady := func(name string) {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name + PodSuffix}, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("runs a CPU-only DevBox without nvidia RuntimeClass or GPU requests", func() {
		box := newTestDevBox("cpu-only", namespace)
		box.Spec.Resources = devpilotv1alpha1.DevBoxResources{
			GPUs:     0,
			CPU:      resource.MustParse("4"),
			Memory:   resource.MustParse("16Gi"),
			ShmRatio: "0.25",
		}
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())
		Expect(pod.Spec.RuntimeClassName).To(BeNil())
		Expect(pod.Spec.NodeSelector).NotTo(HaveKey(DefaultGPUNodeLabel))

		main := pod.Spec.Containers[0]
		Expect(main.Resources.Limits).NotTo(HaveKey(GPUResourceName))
		Expect(main.Resources.Requests).NotTo(HaveKey(GPUResourceName))
		Expect(main.Resources.Requests.Cpu().String()).To(Equal("4"))
		Expect(main.Resources.Limits.Cpu().String()).To(Equal("4"))
		Expect(main.Resources.Requests.Memory().String()).To(Equal("16Gi"))
		Expect(main.Resources.Limits.Memory().String()).To(Equal("16Gi"))

		// 25% of 16Gi is 4Gi.
		Expect(pod.Spec.Volumes).To(ContainElement(Satisfy(func(v corev1.Volume) bool {
			return v.Name == ShmVolumeName &&
				v.EmptyDir != nil &&
				v.EmptyDir.Medium == corev1.StorageMediumMemory &&
				v.EmptyDir.SizeLimit != nil &&
				v.EmptyDir.SizeLimit.Value() == 4*1024*1024*1024
		})))

		markPodReady(box.Name)
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
	})

	It("prepares storage and access for an initially Stopped DevBox, then boots on start action", func() {
		box := newSSHDevBox("cold-stopped", namespace, testClientKey())
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		By("pre-creating the rootdisk PVC and Service without creating a Pod or billing segment")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, &corev1.Service{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, &corev1.Pod{})).NotTo(Succeed())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopped))
		Expect(box.Status.StoppedAt).NotTo(BeNil())
		Expect(box.Status.NonRunningSince).NotTo(BeNil())
		Expect(box.Status.Segments).To(BeEmpty())

		By("starting on demand via devpilot.wutz.dev/action: start")
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		box.Annotations = map[string]string{AnnotationAction: "start=" + epochNow()}
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateRunning))
		Expect(box.Annotations).NotTo(HaveKey(AnnotationAction))

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markPodReady(box.Name)
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
		Expect(box.Status.Segments).To(HaveLen(1))
		Expect(box.Status.Segments[0].EndTime).To(BeNil())
	})

	It("mounts shared PVCs into the main container and forwards custom command, env, zone, nodeSelector and tolerations", func() {
		box := newTestDevBox("custom-mounts", namespace)
		box.Spec.Command = []string{"python", "-m", "vllm.entrypoints.openai.api_server"}
		box.Spec.Args = []string{"--port", "8000"}
		box.Spec.Env = []corev1.EnvVar{{Name: "HF_HOME", Value: "/models/hf"}}
		box.Spec.Zone = scenarioZone
		box.Spec.NodeSelector = map[string]string{
			ZoneLabel:                scenarioZoneOvrd,
			"accelerator.custom/gen": "blackwell",
		}
		box.Spec.Tolerations = []corev1.Toleration{{
			Key:      "nvidia.com/gpu",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}}
		box.Spec.Volumes = []devpilotv1alpha1.DevBoxVolume{
			{
				Name:                  "shared-data",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-datasets"},
				MountPath:             "/workspace/data",
				ReadOnly:              false,
			},
			{
				Name:                  "model-weights",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-models"},
				MountPath:             "/models",
				ReadOnly:              true,
			},
			{
				// Reserved operator volume names must not override internal mounts.
				Name:                  RootDiskVolumeName,
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "ignored-pvc"},
				MountPath:             "/should-not-mount",
			},
		}
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())

		By("attaching and mounting both extra PVCs with their configured ReadOnly flags")
		Expect(pod.Spec.Volumes).To(ContainElement(Satisfy(func(v corev1.Volume) bool {
			return v.Name == "shared-data" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "team-datasets"
		})))
		Expect(pod.Spec.Volumes).To(ContainElement(Satisfy(func(v corev1.Volume) bool {
			return v.Name == "model-weights" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "shared-models"
		})))

		main := pod.Spec.Containers[0]
		Expect(main.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      "shared-data",
			MountPath: "/workspace/data",
			ReadOnly:  false,
		}))
		Expect(main.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      "model-weights",
			MountPath: "/models",
			ReadOnly:  true,
		}))
		Expect(main.VolumeMounts).NotTo(ContainElement(Satisfy(func(m corev1.VolumeMount) bool {
			return m.MountPath == "/should-not-mount"
		})))

		By("forwarding custom command, args, env, scheduling overrides and tolerations")
		Expect(slices.Contains(main.Command, "--port")).To(BeTrue())
		Expect(main.Command[len(main.Command)-2:]).To(Equal([]string{"--port", "8000"}))
		Expect(main.Env).To(ContainElement(corev1.EnvVar{Name: "HF_HOME", Value: "/models/hf"}))
		Expect(pod.Spec.NodeSelector).To(HaveKeyWithValue(DefaultGPUNodeLabel, testGPUModelA100))
		Expect(pod.Spec.NodeSelector).To(HaveKeyWithValue(ZoneLabel, scenarioZoneOvrd))
		Expect(pod.Spec.NodeSelector).To(HaveKeyWithValue("accelerator.custom/gen", "blackwell"))
		Expect(pod.Spec.Tolerations).To(ContainElement(box.Spec.Tolerations[0]))
	})

	It("restarts a Running DevBox, and switches base image after stop + reset-rootdisk", func() {
		box := newTestDevBox("restart-and-reset", namespace)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markPodReady(box.Name)
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		By("restarting the Pod via action: restart without losing the rootdisk claim")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Annotations = map[string]string{AnnotationAction: "restart=" + epochNow()}
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Annotations).NotTo(HaveKey(AnnotationAction))
		Expect(box.Status.Message).To(ContainSubstring("Restarting"))

		By("stopping, resetting the root disk, switching base image, and booting cleanly")
		box.Annotations = map[string]string{AnnotationAction: "stop=" + epochNow()}
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		podKey := types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podKey.Name, Namespace: podKey.Namespace}}))
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopped))

		box.Annotations = map[string]string{AnnotationAction: "reset-rootdisk=" + epochNow()}
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.ActiveImageDigest).To(BeEmpty())
		Expect(box.Status.RootDiskPVC).To(BeEmpty())

		// Simulate kubelet/GC reclaiming the deleted rootdisk PVC before recreation.
		pvcKey := types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix}
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcKey.Name, Namespace: pvcKey.Namespace}}))

		resolver.digestByRef = map[string]string{scenarioImageV2: scenarioDigestV2}
		box.Spec.Image = scenarioImageV2
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(pvcKey)
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.ActiveImage).To(Equal(scenarioImageV2))
		Expect(box.Status.ActiveImageDigest).To(Equal(scenarioDigestV2))
	})

	It("publishes SSH, Jupyter, HTTP and GRPC endpoints cleanly", func() {
		exposed := newExposedReconciler()
		box := newSSHDevBox("multi-access", namespace, testClientKey())
		box.Spec.Access.SSH.Username = "ubuntu"
		box.Spec.Access.Jupyter = &devpilotv1alpha1.JupyterAccess{Enabled: true, Port: 8888}
		box.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{
			{Name: "tensorboard", Port: 6006, Protocol: ProtocolHTTP},
			{Name: "triton", Port: 8001, Protocol: ProtocolGRPC},
		}
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)

		_, err := reconcileOnce(exposed, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(exposed, key)
		Expect(err).NotTo(HaveOccurred())
		markPodReady(box.Name)
		_, err = reconcileOnce(exposed, key)
		Expect(err).NotTo(HaveOccurred())

		By("exposing all ports on the shared Service and splitting HTTP and GRPC Ingresses")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, svc)).To(Succeed())
		Expect(svc.Spec.Ports).To(HaveLen(4))

		grpcIngress := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + IngressGRPCSuffix}, grpcIngress)).To(Succeed())
		Expect(grpcIngress.Annotations).To(HaveKeyWithValue("nginx.ingress.kubernetes.io/backend-protocol", ProtocolGRPC))

		By("recording user-facing endpoints in status")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Endpoints.SSH).To(ContainSubstring("ubuntu@"))
		Expect(box.Status.Endpoints.Jupyter).To(HavePrefix("https://jupyter-"))
		Expect(box.Status.Endpoints.Ports).To(HaveKeyWithValue("tensorboard", HavePrefix("https://tensorboard-")))
		Expect(box.Status.Endpoints.Ports).To(HaveKeyWithValue("triton", HavePrefix("grpcs://triton-")))
	})

	It("serializes concurrent DevBoxCommits, warns on large model cache, and schedules rebase prewarm", func() {
		DeferCleanup(func() { cleanupOfflineCommit(namespace) })
		commitReconciler := newCommitReconciler("")
		recorder := events.NewFakeRecorder(16)
		commitReconciler.Recorder = recorder

		firstKey := newOfflineCommitWith(namespace, "commit-queue-box", func(c *devpilotv1alpha1.DevBoxCommit) {
			c.Spec.RebaseDevBox = true
		})

		box := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "commit-queue-box"}, box)).To(Succeed())
		box.Status.LastNodeName = "gpu-node-a"
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

		second := &devpilotv1alpha1.DevBoxCommit{
			ObjectMeta: metav1.ObjectMeta{Name: "commit-2", Namespace: namespace},
			Spec: devpilotv1alpha1.DevBoxCommitSpec{
				DevBoxName:  "commit-queue-box",
				Destination: devpilotv1alpha1.CommitDestination{Repository: testCommitRepository, Tags: []string{"v2"}},
				Consistency: devpilotv1alpha1.ConsistencyOfflineOnly,
			},
		}
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		secondKey := client.ObjectKeyFromObject(second)
		DeferCleanup(func() {
			cur := &devpilotv1alpha1.DevBoxCommit{}
			if err := k8sClient.Get(ctx, secondKey, cur); err == nil {
				controllerutil.RemoveFinalizer(cur, CommitFinalizer)
				_ = k8sClient.Update(ctx, cur)
				_ = k8sClient.Delete(ctx, cur)
			}
		})

		By("advancing the first commit to Building while keeping the second Pending")
		first := reconcileCommit(commitReconciler, firstKey)
		Expect(first.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseBuilding))

		second = reconcileCommit(commitReconciler, secondKey)
		Expect(second.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhasePending))
		Expect(second.Status.Message).To(ContainSubstring("Waiting for an earlier commit"))

		By("completing the first commit with a >=5Gi model cache warning and rebase prewarm")
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: buildJobName(first)}, job)).To(Succeed())
		largeMsgBytes, marshalErr := json.Marshal(map[string]any{
			"baseDigest":     testDigest,
			"manifestDigest": testCommitDigest,
			"configDigest":   testCommitDigest,
			"diffID":         testCommitDigest,
			"layerSize":      int64(2048),
			"stats":          map[string]any{"modelCacheBytes": int64(6 * 1024 * 1024 * 1024)},
			"images":         []string{testCommitRepository + ":v1"},
		})
		Expect(marshalErr).NotTo(HaveOccurred())
		completeBuildJob(job, string(largeMsgBytes))

		first = reconcileCommit(commitReconciler, firstKey)
		Expect(first.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSucceeded))
		Expect(first.Status.ModelCacheBytes).To(Equal(int64(6 * 1024 * 1024 * 1024)))
		Expect(first.Status.Message).To(ContainSubstring("6.0 GiB"))
		Expect(first.Status.Message).To(ContainSubstring("rebase"))
		Expect(first.Status.Message).To(ContainSubstring("prewarming"))

		Eventually(recorder.Events).Should(Receive(ContainSubstring("LargeModelCache")))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "commit-queue-box"}, box)).To(Succeed())
		Expect(box.Status.PendingRebaseImage).To(Equal(testCommitRepository + ":v1"))

		ipwName := prewarmPodName("commit-"+namespace, first.Name)
		ipw := &devpilotv1alpha1.ImagePrewarm{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ipwName}, ipw)).To(Succeed())
		Expect(ipw.Spec.NodeSelector).To(HaveKeyWithValue(corev1.LabelHostname, "gpu-node-a"))
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, ipw))
		})

		By("letting the second queued commit proceed now that the lock is free")
		second = reconcileCommit(commitReconciler, secondKey)
		Expect(second.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseBuilding))
		Expect(strings.TrimSpace(second.Status.Message)).NotTo(ContainSubstring("Waiting for an earlier commit"))
	})

	It("configures a multi-GPU fine-tuning DevBox with Guaranteed QoS, scaled /dev/shm, separated weights/workspace PVCs, and TensorBoard", func() {
		exposed := newExposedReconciler()
		box := newSSHDevBox("llm-finetune-4gpu", namespace, testClientKey())
		box.Spec.Resources = devpilotv1alpha1.DevBoxResources{
			GPUs:     4,
			GPUModel: testGPUModelA100,
			CPU:      resource.MustParse("32"),
			Memory:   resource.MustParse("128Gi"),
			ShmRatio: "0.5",
		}
		box.Spec.Env = []corev1.EnvVar{
			{Name: "HF_HOME", Value: "/workspace/cache/huggingface"},
			{Name: "TORCH_HOME", Value: "/workspace/cache/torch"},
			{Name: "NCCL_DEBUG", Value: "WARN"},
		}
		box.Spec.Volumes = []devpilotv1alpha1.DevBoxVolume{
			{
				Name:                  "base-models",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-models"},
				MountPath:             "/models",
				ReadOnly:              true,
			},
			{
				Name:                  "finetune-workspace",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-finetune-workspace"},
				MountPath:             "/workspace",
				ReadOnly:              false,
			},
		}
		box.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{
			{Name: "tensorboard", Port: 6006, Protocol: ProtocolHTTP},
		}
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)

		_, err := reconcileOnce(exposed, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(exposed, key)
		Expect(err).NotTo(HaveOccurred())
		markPodReady(box.Name)
		_, err = reconcileOnce(exposed, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())
		Expect(pod.Spec.RuntimeClassName).NotTo(BeNil())
		Expect(*pod.Spec.RuntimeClassName).To(Equal("nvidia"))
		Expect(pod.Spec.NodeSelector).To(HaveKeyWithValue(DefaultGPUNodeLabel, testGPUModelA100))

		By("requesting 4 GPUs and equal CPU/memory limits for Guaranteed QoS")
		main := pod.Spec.Containers[0]
		Expect(main.Resources.Requests[GPUResourceName]).To(Equal(resource.MustParse("4")))
		Expect(main.Resources.Limits[GPUResourceName]).To(Equal(resource.MustParse("4")))
		Expect(main.Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("32")))
		Expect(main.Resources.Limits[corev1.ResourceCPU]).To(Equal(resource.MustParse("32")))
		Expect(main.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("128Gi")))
		Expect(main.Resources.Limits[corev1.ResourceMemory]).To(Equal(resource.MustParse("128Gi")))

		By("sizing /dev/shm to 64Gi (50% of 128Gi) for DataLoader and NCCL")
		Expect(pod.Spec.Volumes).To(ContainElement(Satisfy(func(v corev1.Volume) bool {
			return v.Name == ShmVolumeName &&
				v.EmptyDir != nil &&
				v.EmptyDir.Medium == corev1.StorageMediumMemory &&
				v.EmptyDir.SizeLimit != nil &&
				v.EmptyDir.SizeLimit.Value() == 64*1024*1024*1024
		})))

		By("mounting read-only base weights and writable workspace with cache redirect env vars")
		Expect(main.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      "base-models",
			MountPath: "/models",
			ReadOnly:  true,
		}))
		Expect(main.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      "finetune-workspace",
			MountPath: "/workspace",
			ReadOnly:  false,
		}))
		Expect(main.Env).To(ContainElements(
			corev1.EnvVar{Name: "HF_HOME", Value: "/workspace/cache/huggingface"},
			corev1.EnvVar{Name: "TORCH_HOME", Value: "/workspace/cache/torch"},
			corev1.EnvVar{Name: "NCCL_DEBUG", Value: "WARN"},
		))

		By("publishing TensorBoard and SSH endpoints")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
		Expect(box.Status.Endpoints.SSH).NotTo(BeEmpty())
		Expect(box.Status.Endpoints.Ports).To(HaveKeyWithValue("tensorboard", HavePrefix("https://tensorboard-")))
	})
})
