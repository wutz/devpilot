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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	crdTestStorageNVMe = "block-nvme"
	crdTestImage       = "nvcr.io/nvidia/pytorch:25.08-py3"
	crdTestRepo        = "registry.example.com/team/devbox"
)

var crdTestCounter int

func newMinimalDevBox(name, ns string) *devpilotv1alpha1.DevBox {
	return &devpilotv1alpha1.DevBox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: devpilotv1alpha1.DevBoxSpec{
			Image: crdTestImage,
			RootDisk: devpilotv1alpha1.RootDiskSpec{
				Size:             apiresource.MustParse("50Gi"),
				StorageClassName: crdTestStorageNVMe,
			},
		},
	}
}

func newMinimalCommit(name, ns, devBoxName string) *devpilotv1alpha1.DevBoxCommit {
	return &devpilotv1alpha1.DevBoxCommit{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: devpilotv1alpha1.DevBoxCommitSpec{
			DevBoxName: devBoxName,
			Destination: devpilotv1alpha1.CommitDestination{
				Repository: crdTestRepo,
				Tags:       []string{"v1"},
			},
		},
	}
}

var _ = Describe("CRD OpenAPI, CEL and Shipped Examples Admission", func() {
	var namespace string

	BeforeEach(func() {
		crdTestCounter++
		namespace = fmt.Sprintf("crd-%d-%d", GinkgoRandomSeed(), crdTestCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}))).To(Succeed())
	})

	Context("Shipped examples and samples", func() {
		It("admits every YAML manifest in deploy/examples and config/samples via API server dry-run", func() {
			dirs := []string{
				filepath.Join("..", "..", "..", "deploy", "examples"),
				filepath.Join("..", "..", "..", "config", "samples"),
			}

			validatedFiles := 0
			for _, dir := range dirs {
				entries, err := os.ReadDir(dir)
				Expect(err).NotTo(HaveOccurred())
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == "kustomization.yaml" {
						continue
					}
					path := filepath.Join(dir, entry.Name())
					f, openErr := os.Open(path)
					Expect(openErr).NotTo(HaveOccurred(), "open %s", path)

					decoder := k8syaml.NewYAMLOrJSONDecoder(f, 4096)
					obj := &unstructured.Unstructured{}
					decodeErr := decoder.Decode(obj)
					Expect(f.Close()).To(Succeed())
					Expect(decodeErr).NotTo(HaveOccurred(), "decode %s", path)
					Expect(obj.GetAPIVersion()).To(Equal(devpilotv1alpha1.GroupVersion.String()), "apiVersion in %s", path)

					// ImagePrewarm is cluster-scoped; DevBox and DevBoxCommit live in a namespace.
					if obj.GetKind() != "ImagePrewarm" {
						obj.SetNamespace(namespace)
					} else {
						obj.SetName(fmt.Sprintf("%s-%d", obj.GetName(), crdTestCounter))
					}

					Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(),
						"manifest %s should pass CRD schema, CEL and webhooks", path)
					validatedFiles++
				}
			}
			Expect(validatedFiles).To(BeNumerically(">=", 8))
		})
	})

	Context("DevBox API server schema, CEL and webhook enforcement", func() {
		It("populates CRD schema and webhook defaults for a minimal DevBox", func() {
			raw := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": devpilotv1alpha1.GroupVersion.String(),
					"kind":       "DevBox",
					"metadata": map[string]any{
						"name":      "defaults-box",
						"namespace": namespace,
					},
					"spec": map[string]any{
						"image": crdTestImage,
						"resources": map[string]any{
							"gpus": 0,
						},
						"rootDisk": map[string]any{
							"size":             "50Gi",
							"storageClassName": crdTestStorageNVMe,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, raw)).To(Succeed())

			got := &devpilotv1alpha1.DevBox{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "defaults-box"}, got)).To(Succeed())
			Expect(got.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateRunning))
			Expect(got.Spec.BillingMode).To(Equal(devpilotv1alpha1.BillingModeOnDemand))
			Expect(got.Spec.Resources.CPU).To(Equal(apiresource.MustParse("4")))
			Expect(got.Spec.Resources.Memory).To(Equal(apiresource.MustParse("8Gi")))
			Expect(got.Spec.Resources.ShmRatio).To(Equal("0.5"))
			Expect(got.Spec.RootDisk.RetentionDaysAfterStop).To(Equal(int32(15)))
			Expect(got.Spec.Scheduling.Prepull).To(Equal(devpilotv1alpha1.PrepullPolicyAuto))
		})

		It("rejects invalid user inputs via OpenAPI and CEL rules", func() {
			By("rejecting reserved DEVPILOT_* env variables")
			envBox := newMinimalDevBox("bad-env", namespace)
			envBox.Spec.Env = []corev1.EnvVar{{Name: "DEVPILOT_ROOT_DISK", Value: "/bad"}}
			Expect(k8sClient.Create(ctx, envBox)).NotTo(Succeed())

			By("rejecting invalid pricePerHour formats")
			priceBox := newMinimalDevBox("bad-price", namespace)
			priceBox.Spec.PricePerHour = "1.1234567"
			Expect(k8sClient.Create(ctx, priceBox)).NotTo(Succeed())

			By("rejecting invalid shmRatio formats")
			shmBox := newMinimalDevBox("bad-shm", namespace)
			shmBox.Spec.Resources.ShmRatio = "50%"
			Expect(k8sClient.Create(ctx, shmBox)).NotTo(Succeed())

			By("rejecting GPU count above schema maximum")
			gpuBox := newMinimalDevBox("bad-gpu", namespace)
			gpuBox.Spec.Resources.GPUs = 65
			Expect(k8sClient.Create(ctx, gpuBox)).NotTo(Succeed())

			By("rejecting reserved port names and duplicate port numbers")
			reservedPortBox := newMinimalDevBox("bad-port-name", namespace)
			reservedPortBox.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{{Name: "ssh", Port: 2222}}
			Expect(k8sClient.Create(ctx, reservedPortBox)).NotTo(Succeed())

			dupPortBox := newMinimalDevBox("dup-port-num", namespace)
			dupPortBox.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{
				{Name: "web", Port: 8080},
				{Name: "metrics", Port: 8080},
			}
			Expect(k8sClient.Create(ctx, dupPortBox)).NotTo(Succeed())
		})

		It("enforces update immutability and allows rootDisk expansion", func() {
			box := newMinimalDevBox("immutable-box", namespace)
			Expect(k8sClient.Create(ctx, box)).To(Succeed())
			key := client.ObjectKeyFromObject(box)

			By("rejecting billingMode mutation")
			updated := &devpilotv1alpha1.DevBox{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Spec.BillingMode = devpilotv1alpha1.BillingModeSpot
			Expect(k8sClient.Update(ctx, updated)).NotTo(Succeed())

			By("rejecting storageClassName mutation")
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Spec.RootDisk.StorageClassName = "block-vast-cs"
			Expect(k8sClient.Update(ctx, updated)).NotTo(Succeed())

			By("rejecting rootDisk shrinking")
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Spec.RootDisk.Size = apiresource.MustParse("20Gi")
			Expect(k8sClient.Update(ctx, updated)).NotTo(Succeed())

			By("allowing rootDisk growth")
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Spec.RootDisk.Size = apiresource.MustParse("100Gi")
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			By("protecting spec.image once activeImageDigest is pinned")
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Status.ActiveImage = crdTestImage
			updated.Status.ActiveImageDigest = testPinnedDigest
			Expect(k8sClient.Status().Update(ctx, updated)).To(Succeed())

			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Spec.Image = testOtherImage
			err := k8sClient.Update(ctx, updated)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("commit the devbox or reset the root disk"))
		})
	})

	Context("DevBoxCommit and ImagePrewarm CRD validation", func() {
		It("applies defaults and rejects post-create DevBoxCommit spec edits", func() {
			commit := newMinimalCommit("commit-immutable", namespace, "some-box")
			Expect(k8sClient.Create(ctx, commit)).To(Succeed())

			got := &devpilotv1alpha1.DevBoxCommit{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(commit), got)).To(Succeed())
			Expect(got.Spec.Consistency).To(Equal(devpilotv1alpha1.ConsistencySync))
			Expect(got.Spec.Compression).To(Equal(devpilotv1alpha1.CompressionZstd))
			Expect(got.Spec.CleanPackageCache).To(BeTrue())

			got.Spec.Destination.Tags = []string{"v2"}
			err := k8sClient.Update(ctx, got)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("DevBoxCommit spec is immutable"))
		})

		It("validates DevBoxCommit required fields and enums", func() {
			badCommit := newMinimalCommit("commit-bad-enum", namespace, "some-box")
			badCommit.Spec.Compression = "lz4"
			Expect(k8sClient.Create(ctx, badCommit)).NotTo(Succeed())

			emptyRepo := newMinimalCommit("commit-no-repo", namespace, "some-box")
			emptyRepo.Spec.Destination.Repository = ""
			Expect(k8sClient.Create(ctx, emptyRepo)).NotTo(Succeed())
		})

		It("defaults ImagePrewarm maxConcurrency and rejects invalid values", func() {
			name := fmt.Sprintf("ipw-valid-%d", crdTestCounter)
			ipw := &devpilotv1alpha1.ImagePrewarm{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: devpilotv1alpha1.ImagePrewarmSpec{
					Image:    crdTestImage,
					PinImage: true,
				},
			}
			Expect(k8sClient.Create(ctx, ipw)).To(Succeed())
			DeferCleanup(func() {
				_ = client.IgnoreNotFound(k8sClient.Delete(ctx, ipw))
			})

			got := &devpilotv1alpha1.ImagePrewarm{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ipw), got)).To(Succeed())
			Expect(got.Spec.MaxConcurrency).To(Equal(int32(5)))

			badIPW := &devpilotv1alpha1.ImagePrewarm{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ipw-bad-%d", crdTestCounter)},
				Spec: devpilotv1alpha1.ImagePrewarmSpec{
					Image: "",
				},
			}
			Expect(k8sClient.Create(ctx, badIPW)).NotTo(Succeed())
		})
	})
})
