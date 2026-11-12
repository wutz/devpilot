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
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// BuildNamespaceSuffixDefault is appended to a project namespace to name the
	// namespace that holds the durable copy of its push credential.
	BuildNamespaceSuffixDefault = "-build"

	// CommitRegistrySuffix is appended to the commit name for the short-lived
	// credential the build Job mounts.
	CommitRegistrySuffix = "-registry"

	// LabelProjectNS records the project namespace a durable copy belongs to.
	LabelProjectNS = "devpilot.wutz.dev/project-namespace"
	// LabelCredential records the credential a copy was made from.
	LabelCredential = "devpilot.wutz.dev/credential"
	// AnnotationCredentialSource records where the durable copy lives.
	AnnotationCredentialSource = "devpilot.wutz.dev/credential-source"
)

// buildNamespace is the project's build namespace, or empty when credential
// isolation is off.
func (r *DevBoxCommitReconciler) buildNamespace(commit *devpilotv1alpha1.DevBoxCommit) string {
	suffix := r.Config.BuildNamespaceSuffixMode()
	if suffix == "" {
		return ""
	}
	return commit.Namespace + suffix
}

// syncRegistryCredential makes sure the push credential is in place for a
// build and returns the name the build Job should mount, or empty when the
// operator pushes without one.
//
// A project member holds get, list and watch on secrets in their own
// namespace, so a push token left there is extractable by every member and
// lets them overwrite images they do not own. The durable copy therefore goes
// to <project><suffix>, which is not a member namespace. The build Job cannot
// mount it from there — a pod only mounts secrets of its own namespace, and the
// Job has to run where its source claim is — so a per-commit copy is written
// into the project namespace and deleted as soon as the build ends.
func (r *DevBoxCommitReconciler) syncRegistryCredential(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit,
) (string, error) {
	if r.Config.RegistrySecret == "" {
		return "", nil
	}
	namespace := r.buildNamespace(commit)
	if namespace == "" {
		// Isolation is off: the credential is read where it always was.
		return r.Config.RegistrySecret, nil
	}

	source, err := r.readRegistryCredential(ctx, commit.Namespace, namespace)
	if err != nil {
		return "", err
	}
	if err := r.ensureBuildNamespace(ctx, namespace, commit.Namespace); err != nil {
		return "", err
	}
	durable := credentialCopy(namespace, source.Name, source, map[string]string{
		LabelManagedBy:  ValueManagedBy,
		LabelProjectNS:  commit.Namespace,
		LabelCredential: source.Name,
	}, nil)
	if err := r.upsertCredential(ctx, durable); err != nil {
		return "", err
	}

	name := commit.Name + CommitRegistrySuffix
	transient := credentialCopy(commit.Namespace, name, source, map[string]string{
		LabelManagedBy:  ValueManagedBy,
		CommitJobLabel:  commit.Name,
		LabelCredential: source.Name,
	}, map[string]string{
		AnnotationCredentialSource: namespace + "/" + source.Name,
	})
	// The commit owns the copy so that finishing, failing or being deleted all
	// collect it, even on a reconcile that never reaches the release path.
	if err := controllerutil.SetOwnerReference(commit, transient, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner reference on credential copy %s: %w", name, err)
	}
	if err := r.upsertCredential(ctx, transient); err != nil {
		return "", err
	}
	return name, nil
}

// readRegistryCredential finds the push credential: the project namespace
// first, because that is where it has always lived, then the build namespace,
// which is the stronger arrangement a platform may have adopted.
func (r *DevBoxCommitReconciler) readRegistryCredential(
	ctx context.Context, projectNamespace, buildNamespace string,
) (*corev1.Secret, error) {
	source := &corev1.Secret{}
	key := types.NamespacedName{Namespace: projectNamespace, Name: r.Config.RegistrySecret}
	err := r.Get(ctx, key, source)
	if err == nil {
		return source, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read registry secret %s: %w", key, err)
	}
	key.Namespace = buildNamespace
	if err := r.Get(ctx, key, source); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("registry secret %s does not exist in %s or %s",
				r.Config.RegistrySecret, projectNamespace, buildNamespace)
		}
		return nil, fmt.Errorf("read registry secret %s: %w", key, err)
	}
	return source, nil
}

// credentialCopy renders a secret carrying the source payload. Only the payload
// travels: annotations on the source can name the helm release that installed
// it, and a copy wearing that release's metadata would be adopted by a later
// upgrade of the source release.
func credentialCopy(
	namespace, name string, source *corev1.Secret, labels, annotations map[string]string,
) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Labels:      labels,
			Annotations: annotations,
		},
		Type: source.Type,
		Data: source.Data,
	}
}

// ensureBuildNamespace creates the build namespace on first use, labelled with
// the project it belongs to so the platform can find them.
func (r *DevBoxCommitReconciler) ensureBuildNamespace(ctx context.Context, name, projectNamespace string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get build namespace %s: %w", name, err)
	}
	created := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				LabelManagedBy: ValueManagedBy,
				LabelProjectNS: projectNamespace,
			},
		},
	}
	if err := r.Create(ctx, created); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create build namespace %s: %w", name, err)
	}
	logf.FromContext(ctx).Info("Created build namespace", "namespace", name, "project", projectNamespace)
	return nil
}

// upsertCredential writes the copy, replacing an existing one whose payload or
// type drifted from the source. Immutability is cleared because a rotated
// docker config has to land in place.
func (r *DevBoxCommitReconciler) upsertCredential(ctx context.Context, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create credential copy %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get credential copy %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	if existing.Type == desired.Type && sameSecretData(existing.Data, desired.Data) &&
		len(existing.OwnerReferences) == len(desired.OwnerReferences) {
		return nil
	}
	existing.Immutable = nil
	existing.Type = desired.Type
	existing.Data = desired.Data
	existing.OwnerReferences = desired.OwnerReferences
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	maps.Copy(existing.Labels, desired.Labels)
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("refresh credential copy %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	logf.FromContext(ctx).Info("Refreshed credential copy", "namespace", desired.Namespace, "name", desired.Name)
	return nil
}

// releaseRegistryCredential removes the per-commit copy once its build is over.
func (r *DevBoxCommitReconciler) releaseRegistryCredential(ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit) error {
	if r.Config.RegistrySecret == "" || r.buildNamespace(commit) == "" {
		return nil
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: commit.Namespace,
		Name:      commit.Name + CommitRegistrySuffix,
	}}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete credential copy %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return nil
}

// sameSecretData reports whether two secret payloads are identical.
func sameSecretData(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		other, ok := b[key]
		if !ok || string(other) != string(value) {
			return false
		}
	}
	return true
}

// validBuildNamespaceSuffix rejects a suffix that would fold into the project
// namespace or produce an illegal name.
func validBuildNamespaceSuffix(suffix string) bool {
	return !strings.ContainsAny(suffix, " /:")
}
