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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	testCommitImage = "registry.example.com/devpilot-commit:test"
	testCommitName  = "commit-1"
	testCommitTag   = "v1"
	testSetEnvPair  = "EIN=1"
	// testRegistrySecret is the docker config a build pushes with.
	testRegistrySecret = "registry-creds"
	// testCommitDigest is a second digest, so the rebase tests can tell the
	// committed image apart from the base one without a real registry.
	testCommitDigest = "sha256:aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
)

// newOfflineCommit creates a stopped DevBox with a bound root disk claim plus a
// commit that targets it. Nothing is running, so the commit takes the offline
// path; the returned key is the commit to reconcile.
func newOfflineCommit(namespace, devboxName string) types.NamespacedName {
	return newOfflineCommitWith(namespace, devboxName, nil)
}

// newOfflineCommitWith is newOfflineCommit with a hook to shape the commit
// before it is created: its spec is immutable, so flags like rebaseDevBox can
// only be set here.
func newOfflineCommitWith(namespace, devboxName string, mutate func(*devpilotv1alpha1.DevBoxCommit)) types.NamespacedName {
	box := newTestDevBox(devboxName, namespace)
	Expect(k8sClient.Create(ctx, box)).To(Succeed())

	pvcName := devboxName + RootDiskPVCSuffix
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(testVolumeSize)},
			},
		},
	}
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())

	box.Status.Phase = devpilotv1alpha1.PhaseStopped
	box.Status.RootDiskPVC = pvcName
	Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

	commit := &devpilotv1alpha1.DevBoxCommit{
		ObjectMeta: metav1.ObjectMeta{Name: testCommitName, Namespace: namespace},
		Spec: devpilotv1alpha1.DevBoxCommitSpec{
			DevBoxName: devboxName,
			Destination: devpilotv1alpha1.CommitDestination{
				Repository: testCommitRepository,
				Tags:       []string{testCommitTag},
			},
			Compression:       devpilotv1alpha1.CompressionZstd,
			ExcludePaths:      []string{"tmp/scratch"},
			CleanPackageCache: true,
			Changes: devpilotv1alpha1.CommitChanges{
				Author:  "alice",
				Comment: testCommitTag,
				Env:     []devpilotv1alpha1.NameValue{{Name: "EIN", Value: "1"}},
				Cmd:     []string{"python", "train.py"},
			},
		},
	}
	if mutate != nil {
		mutate(commit)
	}
	Expect(k8sClient.Create(ctx, commit)).To(Succeed())
	return client.ObjectKeyFromObject(commit)
}

func newCommitReconciler(registrySecret string) *DevBoxCommitReconciler {
	return &DevBoxCommitReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		Config: Config{InitImage: testInit, CommitImage: testCommitImage, RegistrySecret: registrySecret},
	}
}

// newIsolatingReconciler is newCommitReconciler with credential isolation on:
// the push credential is copied out of the project namespace before the build.
func newIsolatingReconciler() *DevBoxCommitReconciler {
	r := newCommitReconciler(testRegistrySecret)
	r.Config.BuildNamespaceSuffix = BuildNamespaceSuffixDefault
	return r
}

// dockerConfigSecret is a push credential named testRegistrySecret with the
// given payload, in the namespace a build would find it.
func dockerConfigSecret(namespace, payload string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testRegistrySecret, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(payload)},
	}
}

func reconcileCommit(r *DevBoxCommitReconciler, key types.NamespacedName) *devpilotv1alpha1.DevBoxCommit {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	Expect(err).NotTo(HaveOccurred())
	commit := &devpilotv1alpha1.DevBoxCommit{}
	Expect(k8sClient.Get(ctx, key, commit)).To(Succeed())
	return commit
}

// completeBuildJob stands in for the kubelet: it marks the Job succeeded and
// leaves a pod whose container state carries the termination message.
func completeBuildJob(job *batchv1.Job, message string) {
	job.Status.Succeeded = 1
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-pod",
			Namespace: job.Namespace,
			Labels:    map[string]string{batchv1.JobNameLabel: job.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: CommitContainerName, Image: testCommitImage}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: CommitContainerName,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message},
		},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// buildResultMessage is the JSON summary a successful devpilot-commit run
// writes, for tests that care about the finished commit rather than its fields.
func buildResultMessage(baseDigest, image string) string {
	message, err := json.Marshal(map[string]any{
		"baseDigest":     baseDigest,
		"manifestDigest": testCommitDigest,
		"configDigest":   testCommitDigest,
		"diffID":         testCommitDigest,
		"layerSize":      int64(1024),
		"images":         []string{image},
	})
	Expect(err).NotTo(HaveOccurred())
	return string(message)
}

func cleanupOfflineCommit(namespace string) {
	commit := &devpilotv1alpha1.DevBoxCommit{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: testCommitName}, commit); err == nil {
		controllerutil.RemoveFinalizer(commit, CommitFinalizer)
		_ = k8sClient.Update(ctx, commit)
		_ = k8sClient.Delete(ctx, commit)
	}
	_ = k8sClient.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(namespace))
	_ = k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(namespace))
}

var _ = Describe("DevBoxCommit Controller", func() {
	Context("offline commits", func() {
		var namespace string

		BeforeEach(func() {
			namespace = fmt.Sprintf("commit-%d", specCounter)
			specCounter++
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			cleanupOfflineCommit(namespace)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
			_ = k8sClient.Delete(ctx, ns)
		})

		It("starts an isolated build Job for a stopped DevBox", func() {
			key := newOfflineCommit(namespace, "devbox-offline")

			commit := reconcileCommit(newCommitReconciler(""), key)
			Expect(commit.Finalizers).To(ContainElement(CommitFinalizer))
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseBuilding))
			Expect(commit.Status.Mode).To(Equal(devpilotv1alpha1.CommitModeOfflinePVC))
			Expect(commit.Status.BuildJobName).To(Equal(testCommitName + CommitJobSuffix))
			Expect(commit.Status.StartTime).NotTo(BeNil())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			Expect(job.Spec.BackoffLimit).To(HaveValue(BeEquivalentTo(0)))

			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal(CommitContainerName))
			Expect(container.Image).To(Equal(testCommitImage))
			Expect(container.Args).To(ContainElements(
				"--repository="+testCommitRepository,
				"--tags="+testCommitTag,
				"--compression=zstd",
				"--exclude=tmp/scratch",
				"--clean-package-cache=true",
				"--author=alice",
				"--env=EIN=1",
				`--cmd=["python","train.py"]`,
			))
			Expect(container.VolumeMounts).To(ContainElement(HaveField("MountPath", RootDiskMountPath)))
			for _, mount := range container.VolumeMounts {
				if mount.MountPath == RootDiskMountPath {
					Expect(mount.ReadOnly).To(BeTrue(), "the source root disk must be mounted read-only")
				}
			}

			pod := job.Spec.Template.Spec
			Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
			// The snapshot carries a root-owned overlay; a non-root build
			// cannot open upper/ or .meta/base-image.json at all.
			Expect(pod.SecurityContext.RunAsNonRoot).To(HaveValue(BeFalse()))
			Expect(pod.SecurityContext.RunAsUser).To(HaveValue(BeEquivalentTo(0)))
			Expect(container.SecurityContext.Capabilities.Drop).To(Equal([]corev1.Capability{capabilityAll}))
			// Dropping every capability also denies root the read bypass the
			// scan needs for private directories such as apt's 0700 lists/.
			Expect(container.SecurityContext.Capabilities.Add).To(Equal([]corev1.Capability{capabilityDACReadSearch}))
			Expect(container.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
		})

		It("mounts the registry secret as a docker config", func() {
			secretName := testRegistrySecret
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
				Type:       corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`),
				},
			})).To(Succeed())
			key := newOfflineCommit(namespace, "devbox-secret")

			reconcileCommit(newCommitReconciler(secretName), key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "DOCKER_CONFIG", Value: CommitRegistryMount}))
			Expect(container.VolumeMounts).To(ContainElement(HaveField("MountPath", CommitRegistryMount)))
		})

		It("keeps the durable copy of the push credential out of the project namespace", func() {
			secretName := testRegistrySecret
			payload := `{"auths":{"registry.example.com":{"password":"s3cret"}}}`
			Expect(k8sClient.Create(ctx, dockerConfigSecret(namespace, payload))).To(Succeed())
			buildNS := namespace + BuildNamespaceSuffixDefault
			defer func() {
				_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: buildNS}})
			}()
			key := newOfflineCommit(namespace, "devbox-isolated")

			reconcileCommit(newIsolatingReconciler(), key)

			durable := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: buildNS, Name: secretName}, durable)).
				To(Succeed(), "the durable copy lives in the build namespace")
			Expect(durable.Data).To(Equal(map[string][]byte{corev1.DockerConfigJsonKey: []byte(payload)}))
			Expect(durable.Labels).To(HaveKeyWithValue(LabelProjectNS, namespace))
			Expect(durable.Labels).To(HaveKeyWithValue(LabelCredential, secretName))
			Expect(durable.Annotations).NotTo(HaveKey("meta.helm.sh/release-name"),
				"a copy wearing the source release's annotations would be adopted by a later upgrade")

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: buildNS}, ns)).To(Succeed())
			Expect(ns.Labels).To(HaveKeyWithValue(LabelProjectNS, namespace))
		})

		It("mounts a per-commit copy from the project namespace and deletes it with the Job", func() {
			secretName := testRegistrySecret
			Expect(k8sClient.Create(ctx, dockerConfigSecret(namespace, `{"auths":{}}`))).To(Succeed())
			buildNS := namespace + BuildNamespaceSuffixDefault
			defer func() {
				_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: buildNS}})
			}()
			key := newOfflineCommit(namespace, "devbox-transit")
			reconciler := newIsolatingReconciler()

			reconcileCommit(reconciler, key)

			transitName := testCommitName + CommitRegistrySuffix
			transit := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: transitName}, transit)).To(Succeed())
			Expect(transit.Annotations).To(HaveKeyWithValue(AnnotationCredentialSource, buildNS+"/"+secretName))
			Expect(transit.OwnerReferences).To(HaveLen(1))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			var mounted string
			for _, vol := range job.Spec.Template.Spec.Volumes {
				if vol.Name == "registry" {
					mounted = vol.Secret.SecretName
				}
			}
			Expect(mounted).To(Equal(transitName), "the build mounts the short-lived copy")

			commit := &devpilotv1alpha1.DevBoxCommit{}
			Expect(k8sClient.Get(ctx, key, commit)).To(Succeed())
			Expect(k8sClient.Delete(ctx, commit)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: transitName}, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the token must not outlive the build")
			durable := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: buildNS, Name: secretName}, durable)).
				To(Succeed(), "the durable copy stays for the next build")
		})

		It("refreshes the durable copy when the source credential rotates", func() {
			secretName := testRegistrySecret
			source := dockerConfigSecret(namespace, `{"auths":{"v1":{}}}`)
			Expect(k8sClient.Create(ctx, source)).To(Succeed())
			buildNS := namespace + BuildNamespaceSuffixDefault
			defer func() {
				_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: buildNS}})
			}()
			reconciler := newIsolatingReconciler()
			reconcileCommit(reconciler, newOfflineCommit(namespace, "devbox-rotate"))

			rotated := `{"auths":{"v2":{}}}`
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, source)).To(Succeed())
			source.Data[corev1.DockerConfigJsonKey] = []byte(rotated)
			Expect(k8sClient.Update(ctx, source)).To(Succeed())

			// The next build picks the rotation up rather than serving the old
			// token. It is driven directly because the first commit still holds
			// the per-DevBox build slot, which would park a second one in Pending.
			commit := &devpilotv1alpha1.DevBoxCommit{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName,
			}, commit)).To(Succeed())
			_, err := reconciler.syncRegistryCredential(ctx, commit)
			Expect(err).NotTo(HaveOccurred())

			durable := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: buildNS, Name: secretName}, durable)).To(Succeed())
			Expect(durable.Data[corev1.DockerConfigJsonKey]).To(Equal([]byte(rotated)))
		})

		It("reads the credential from the build namespace when the project namespace has none", func() {
			buildNS := namespace + BuildNamespaceSuffixDefault
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: buildNS},
			})).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: buildNS}})
			}()
			Expect(k8sClient.Create(ctx, dockerConfigSecret(buildNS, `{"auths":{"build":{}}}`))).To(Succeed())

			key := newOfflineCommit(namespace, "devbox-buildns")
			commit := reconcileCommit(newIsolatingReconciler(), key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseBuilding))

			transit := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitRegistrySuffix,
			}, transit)).To(Succeed())
		})

		It("fails the commit when the push credential is missing everywhere", func() {
			key := newOfflineCommit(namespace, "devbox-nocred")
			buildNS := namespace + BuildNamespaceSuffixDefault
			defer func() {
				_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: buildNS}})
			}()

			commit := reconcileCommit(newIsolatingReconciler(), key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
			Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "RegistryCredentialFailed")))
		})

		It("records the pushed image when the Job succeeds", func() {
			key := newOfflineCommit(namespace, "devbox-success")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			image := testCommitRepository + "@" + testCommitDigest
			message, err := json.Marshal(map[string]any{
				"baseDigest":     testDigest,
				"manifestDigest": testCommitDigest,
				"configDigest":   testCommitDigest,
				"diffID":         testCommitDigest,
				"layerSize":      int64(2048),
				"images":         []string{image},
			})
			Expect(err).NotTo(HaveOccurred())
			completeBuildJob(job, string(message))

			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSucceeded))
			Expect(commit.Status.Digest).To(Equal(testCommitDigest))
			Expect(commit.Status.Images).To(Equal([]string{image}))
			Expect(commit.Status.LayerSize).To(Equal(int64(2048)))
			Expect(commit.Status.BaseImageDigest).To(Equal(testDigest))
			Expect(commit.Status.CompletionTime).NotTo(BeNil())
			Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "Pushed")))
		})

		It("schedules a rebase onto the committed image", func() {
			key := newOfflineCommitWith(namespace, "devbox-rebase", func(c *devpilotv1alpha1.DevBoxCommit) {
				c.Spec.RebaseDevBox = true
			})
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			image := testCommitRepository + "@" + testCommitDigest
			completeBuildJob(job, buildResultMessage(testCommitDigest, image))

			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSucceeded))
			Expect(commit.Status.Message).To(ContainSubstring("will rebase onto it on its next start"))

			box := &devpilotv1alpha1.DevBox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: "devbox-rebase",
			}, box)).To(Succeed())
			Expect(box.Status.PendingRebaseImage).To(Equal(image))
			Expect(box.Spec.Image).To(Equal(testDevBoxImage), "the spec moves only once the rebase ran")
		})

		It("leaves the pending image alone for a plain commit", func() {
			key := newOfflineCommit(namespace, "devbox-norebase")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			completeBuildJob(job, buildResultMessage(testDigest, testCommitRepository+"@"+testCommitDigest))

			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSucceeded))
			Expect(commit.Status.Message).NotTo(ContainSubstring("rebase"))
			Expect(commit.Status.Message).To(Equal("Pushed " + testCommitRepository + "@" + testCommitDigest))

			box := &devpilotv1alpha1.DevBox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: "devbox-norebase",
			}, box)).To(Succeed())
			Expect(box.Status.PendingRebaseImage).To(BeEmpty())
		})

		// A pod that appears after the build started is the one window
		// resolveSource cannot see: it runs before the Job is observed as
		// finished, so finishBuild checks the disk again before scheduling.
		It("skips the rebase when a pod took the root disk during the build", func() {
			key := newOfflineCommitWith(namespace, "devbox-rebase-live", func(c *devpilotv1alpha1.DevBoxCommit) {
				c.Spec.RebaseDevBox = true
			})
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			Expect(k8sClient.Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "devbox-rebase-live" + PodSuffix, Namespace: namespace},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: MainContainerName, Image: testDevBoxImage}},
				},
			})).To(Succeed())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			completeBuildJob(job, buildResultMessage(testDigest, testCommitRepository+"@"+testCommitDigest))

			commit := &devpilotv1alpha1.DevBoxCommit{}
			Expect(k8sClient.Get(ctx, key, commit)).To(Succeed())
			Expect(reconciler.finishBuild(ctx, commit, job)).To(Succeed())
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSucceeded))
			Expect(commit.Status.Message).To(ContainSubstring("started before the build finished"))

			box := &devpilotv1alpha1.DevBox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: "devbox-rebase-live",
			}, box)).To(Succeed())
			Expect(box.Status.PendingRebaseImage).To(BeEmpty())
		})

		It("fails when the build Job reports an error", func() {
			key := newOfflineCommit(namespace, "devbox-failed")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			completeBuildJob(job, `{"error":"the registry refused the push"}`)

			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
			Expect(commit.Status.Message).To(ContainSubstring("refused"))
			Expect(commit.Status.Digest).To(BeEmpty())
		})

		// A failed Job keeps its pod, that pod mounts the source claim, and a
		// Job owned by a commit that is never deleted is never collected. The
		// claim would then sit in Terminating for good; observed on bz1-dev1.
		It("releases the build Job when the build fails", func() {
			key := newOfflineCommit(namespace, "devbox-failed-release")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			completeBuildJob(job, `{"error":"the registry refused the push"}`)

			reconcileCommit(reconciler, key)

			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, &batchv1.Job{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the failed build Job must be released")
		})

		// Deleting the Job wakes the reconciler through Owns. If that copy of the
		// commit still reads Building, it starts a second Job that can never
		// schedule; nothing else would ever release it.
		It("releases a build Job that reappears after the terminal phase", func() {
			key := newOfflineCommit(namespace, "devbox-phantom-job")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			completeBuildJob(job, `{"error":"the registry refused the push"}`)
			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))

			// Stand in for the stale reconcile: a Job named exactly as the one
			// buildJob() would create, on a commit that is already terminal.
			phantom := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testCommitName + CommitJobSuffix,
					Namespace: namespace,
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "build", Image: testCommitImage}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, phantom)).To(Succeed())

			reconcileCommit(reconciler, key)

			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, &batchv1.Job{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a Job left on a terminal commit must be released")
		})

		It("fails when the result message is missing", func() {
			key := newOfflineCommit(namespace, "devbox-nomessage")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
			Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "ResultMissing")))
		})

		It("waits instead of starting a build when the message is unreadable", func() {
			key := newOfflineCommit(namespace, "devbox-garbage")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitJobSuffix,
			}, job)).To(Succeed())
			completeBuildJob(job, "{not json")

			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
			Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "ResultUnreadable")))
		})

		It("adds its finalizer without sending the immutable spec back", func() {
			// The spec is immutable by CEL, so a full Update of the typed object
			// fails on a commit written from a manifest: the client renders the
			// optional changes struct, and the stored spec has no changes key.
			// Reaching Building here proves the finalizer went out as a metadata
			// patch rather than as part of a spec round trip.
			key := newOfflineCommit(namespace, "devbox-finalizer")

			commit := reconcileCommit(newCommitReconciler(""), key)

			Expect(commit.Finalizers).To(ContainElement(CommitFinalizer))
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseBuilding))
		})

		It("reuses the existing Job instead of starting a second build", func() {
			key := newOfflineCommit(namespace, "devbox-idempotent")
			reconciler := newCommitReconciler("")
			first := reconcileCommit(reconciler, key)
			second := reconcileCommit(reconciler, key)

			Expect(second.Status.BuildJobName).To(Equal(first.Status.BuildJobName))
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(namespace))).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1))
		})

		It("waits while a pod still holds the root disk", func() {
			key := newOfflineCommit(namespace, "devbox-running")
			box := &devpilotv1alpha1.DevBox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: "devbox-running",
			}, box)).To(Succeed())
			box.Status.Phase = devpilotv1alpha1.PhaseRunning
			Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "devbox-running" + PodSuffix, Namespace: namespace},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: MainContainerName, Image: testDevBoxImage}},
				},
			})).To(Succeed())

			commit := reconcileCommit(newCommitReconciler(""), key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhasePending))
			Expect(commit.Status.Message).To(ContainSubstring("terminate"))

			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(namespace))).To(Succeed())
			Expect(jobs.Items).To(BeEmpty())
		})

		It("fails when no commit image is configured", func() {
			key := newOfflineCommit(namespace, "devbox-noimage")
			reconciler := newCommitReconciler("")
			reconciler.Config.CommitImage = ""

			commit := reconcileCommit(reconciler, key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
			Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "CommitDisabled")))
		})

		It("fails when the root disk claim is missing", func() {
			key := newOfflineCommit(namespace, "devbox-nodisk")
			box := &devpilotv1alpha1.DevBox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: "devbox-nodisk",
			}, box)).To(Succeed())
			// Point at a claim that was never created: deleting the real one is
			// asynchronous in envtest, so it would still be readable here.
			box.Status.RootDiskPVC = "never-created" + RootDiskPVCSuffix
			Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

			commit := reconcileCommit(newCommitReconciler(""), key)
			Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
			Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "RootDiskMissing")))
		})

		It("releases the build Job on deletion", func() {
			key := newOfflineCommit(namespace, "devbox-delete")
			reconciler := newCommitReconciler("")
			reconcileCommit(reconciler, key)

			commit := &devpilotv1alpha1.DevBoxCommit{}
			Expect(k8sClient.Get(ctx, key, commit)).To(Succeed())
			Expect(k8sClient.Delete(ctx, commit)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, key, &devpilotv1alpha1.DevBoxCommit{})
			Expect(err).To(HaveOccurred())
		})
	})
})
