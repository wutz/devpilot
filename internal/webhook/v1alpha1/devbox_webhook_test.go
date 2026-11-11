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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

// newDevBox returns a minimal valid DevBox for the validator tests.
func newDevBox() *devpilotv1alpha1.DevBox {
	return &devpilotv1alpha1.DevBox{
		Spec: devpilotv1alpha1.DevBoxSpec{
			Image:       "pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime",
			BillingMode: devpilotv1alpha1.BillingModeOnDemand,
			RootDisk: devpilotv1alpha1.RootDiskSpec{
				Size:             apiresource.MustParse("100Gi"),
				StorageClassName: "block-nvme",
			},
		},
	}
}

var _ = Describe("DevBox Webhook", func() {
	var (
		ctx       context.Context
		obj       *devpilotv1alpha1.DevBox
		validator DevBoxValidator
		defaulter DevBoxDefaulter
	)

	BeforeEach(func() {
		ctx = context.Background()
		obj = newDevBox()
		validator = DevBoxValidator{}
		defaulter = DevBoxDefaulter{}
	})

	Context("When defaulting", func() {
		It("fills the power state and billing mode", func() {
			obj.Spec.DesiredState = ""
			obj.Spec.BillingMode = ""
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateRunning))
			Expect(obj.Spec.BillingMode).To(Equal(devpilotv1alpha1.BillingModeOnDemand))
		})

		It("never overrides an explicit stopped state", func() {
			obj.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateStopped))
		})
	})

	Context("When creating", func() {
		It("admits a valid DevBox", func() {
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("rejects a missing base image", func() {
			obj.Spec.Image = " "
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.image: Required value`))
		})

		It("rejects a missing storage class", func() {
			obj.Spec.RootDisk.StorageClassName = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rootDisk.storageClassName"))
		})

		It("rejects a zero root disk size", func() {
			obj.Spec.RootDisk.Size = apiresource.Quantity{}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rootDisk.size"))
		})

		It("rejects reserved environment variables", func() {
			obj.Spec.Env = []corev1.EnvVar{{Name: "DEVPILOT_BASE_IMAGE_DIGEST", Value: "sha256:deadbeef"}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("reserved for the operator"))
		})

		It("accepts a user variable that merely mentions the prefix", func() {
			obj.Spec.Env = []corev1.EnvVar{{Name: "MY_DEVPILOT_FLAG", Value: "1"}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects a volume without a claim", func() {
			obj.Spec.Volumes = []devpilotv1alpha1.DevBoxVolume{{Name: "data", MountPath: "/data"}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("persistentVolumeClaim"))
		})
	})

	Context("When updating", func() {
		var oldObj *devpilotv1alpha1.DevBox

		BeforeEach(func() {
			oldObj = newDevBox()
		})

		It("allows changing the image before the root disk exists", func() {
			obj.Spec.Image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("protects the image once a digest is pinned", func() {
			oldObj.Status.ActiveImage = oldObj.Spec.Image
			oldObj.Status.ActiveImageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			obj.Spec.Image = "nvidia/cuda:12.4.1-devel-ubuntu22.04"

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("commit the devbox or reset the root disk"))
		})

		It("allows unrelated edits while the image stays put", func() {
			oldObj.Status.ActiveImageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			obj.Spec.Resources.GPUs = 1
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects billingMode changes", func() {
			obj.Spec.BillingMode = devpilotv1alpha1.BillingModeSpot
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("billingMode is immutable"))
		})

		It("rejects storage class changes", func() {
			obj.Spec.RootDisk.StorageClassName = "block-vast-cs"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("storageClassName is immutable"))
		})

		It("rejects shrinking the root disk", func() {
			obj.Spec.RootDisk.Size = apiresource.MustParse("50Gi")
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("may only grow"))
		})

		It("allows growing the root disk", func() {
			obj.Spec.RootDisk.Size = apiresource.MustParse("200Gi")
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
