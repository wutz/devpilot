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
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var devboxlog = logf.Log.WithName("devbox-resource")

// reservedEnvPrefix is the namespace devpilot-init owns; the CEL rule rejects
// it too, but a webhook gives a far better message.
const reservedEnvPrefix = "DEVPILOT_"

// SetupDevBoxWebhookWithManager registers the webhook for DevBox in the manager.
func SetupDevBoxWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &devpilotv1alpha1.DevBox{}).
		WithValidator(&DevBoxValidator{}).
		WithDefaulter(&DevBoxDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-devpilot-wutz-dev-v1alpha1-devbox,mutating=true,failurePolicy=fail,sideEffects=None,groups=devpilot.wutz.dev,resources=devboxes,verbs=create;update,versions=v1alpha1,name=mdevbox-v1alpha1.kb.io,admissionReviewVersions=v1

// DevBoxDefaulter sets the defaults the CRD schema cannot express, and keeps
// the zero values meaningful for clients that bypass the schema defaults.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type DevBoxDefaulter struct{}

// Default implements admission.Defaulter so a webhook will be registered for the Kind DevBox.
func (d *DevBoxDefaulter) Default(_ context.Context, obj *devpilotv1alpha1.DevBox) error {
	if obj.Spec.DesiredState == "" {
		obj.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
	}
	if obj.Spec.BillingMode == "" {
		obj.Spec.BillingMode = devpilotv1alpha1.BillingModeOnDemand
	}
	if obj.Spec.Access.SSH != nil && obj.Spec.Access.SSH.Username == "" {
		obj.Spec.Access.SSH.Username = "root"
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-devpilot-wutz-dev-v1alpha1-devbox,mutating=false,failurePolicy=fail,sideEffects=None,groups=devpilot.wutz.dev,resources=devboxes,verbs=create;update,versions=v1alpha1,name=vdevbox-v1alpha1.kb.io,admissionReviewVersions=v1

// DevBoxValidator enforces the invariants that protect the persistent root
// disk. The CEL rules in the CRD cover what they can see in the spec; the base
// image pin lives in status, so only a webhook can protect it.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type DevBoxValidator struct{}

var devBoxGroupKind = schema.GroupKind{
	Group: devpilotv1alpha1.GroupVersion.Group,
	Kind:  "DevBox",
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type DevBox.
func (v *DevBoxValidator) ValidateCreate(_ context.Context, obj *devpilotv1alpha1.DevBox) (admission.Warnings, error) {
	devboxlog.V(1).Info("Validating DevBox on creation", "name", obj.GetName())
	if errs := validateDevBoxSpec(obj); len(errs) > 0 {
		return nil, apierrors.NewInvalid(devBoxGroupKind, obj.GetName(), errs)
	}
	return nil, nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type DevBox.
func (v *DevBoxValidator) ValidateUpdate(_ context.Context, oldObj, newObj *devpilotv1alpha1.DevBox) (admission.Warnings, error) {
	devboxlog.V(1).Info("Validating DevBox on update", "name", newObj.GetName())

	errs := validateDevBoxSpec(newObj)
	errs = append(errs, validateImmutableFields(oldObj, newObj)...)
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(devBoxGroupKind, newObj.GetName(), errs)
	}
	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type DevBox.
func (v *DevBoxValidator) ValidateDelete(_ context.Context, obj *devpilotv1alpha1.DevBox) (admission.Warnings, error) {
	devboxlog.V(1).Info("Validating DevBox on deletion", "name", obj.GetName())
	// Deleting a DevBox releases the GPU and, through garbage collection, the
	// root disk. Nothing to block on.
	return nil, nil
}

// validateDevBoxSpec checks the stateless rules. They mirror the CRD's CEL
// rules so the same message reaches the user whether or not the webhook is
// installed.
func validateDevBoxSpec(obj *devpilotv1alpha1.DevBox) field.ErrorList {
	var errs field.ErrorList
	spec := &obj.Spec

	if strings.TrimSpace(spec.Image) == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "image"), "a base image is required"))
	}
	if spec.RootDisk.StorageClassName == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "rootDisk", "storageClassName"),
			"an RWO block storage class is required"))
	}
	if spec.RootDisk.Size.IsZero() {
		errs = append(errs, field.Required(field.NewPath("spec", "rootDisk", "size"),
			"a root disk size is required"))
	}
	for i, env := range spec.Env {
		if strings.HasPrefix(env.Name, reservedEnvPrefix) {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec", "env").Index(i).Child("name"),
				fmt.Sprintf("%s* names are reserved for the operator", reservedEnvPrefix)))
		}
	}
	for i, volume := range spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			errs = append(errs, field.Required(field.NewPath("spec", "volumes").Index(i).Child("persistentVolumeClaim"),
				"only persistentVolumeClaim volumes are supported"))
		}
	}
	return errs
}

// validateImmutableFields rejects changes that would invalidate the data in
// the root disk's upper layer.
func validateImmutableFields(oldObj, newObj *devpilotv1alpha1.DevBox) field.ErrorList {
	var errs field.ErrorList

	// status.activeImageDigest means upper/ was populated from that base image.
	// The overlay stores absolute library paths and hardlinks, so mounting it
	// over a different base silently corrupts the devbox. The operator's own
	// rebase is the exception: the new image is the digest the commit published
	// and devpilot-init clears upper/ before it runs, so the move is safe.
	rebase := newObj.Status.PendingRebaseImage != "" && newObj.Spec.Image == newObj.Status.PendingRebaseImage
	if oldObj.Status.ActiveImageDigest != "" && oldObj.Spec.Image != newObj.Spec.Image && !rebase {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "image"),
			fmt.Sprintf("the root disk was created from %s (%s); commit the devbox or reset the root disk before changing the base image",
				oldObj.Status.ActiveImage, oldObj.Status.ActiveImageDigest)))
	}
	if oldObj.Spec.BillingMode != newObj.Spec.BillingMode {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "billingMode"),
			"billingMode is immutable after creation"))
	}
	if oldObj.Spec.RootDisk.StorageClassName != newObj.Spec.RootDisk.StorageClassName {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "rootDisk", "storageClassName"),
			"storageClassName is immutable; reset the root disk to move it"))
	}
	if newObj.Spec.RootDisk.Size.Cmp(oldObj.Spec.RootDisk.Size) < 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "rootDisk", "size"),
			newObj.Spec.RootDisk.Size.String(), "the root disk may only grow"))
	}
	return errs
}
