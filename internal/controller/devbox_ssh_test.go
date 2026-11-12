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
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
	"github.com/wutz/devpilot/internal/sshhost"
)

// testClientKey returns a fresh authorized_keys line, used as a stand-in for a
// user's public key.
func testClientKey() string {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	signer, err := ssh.NewSignerFromKey(key)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// newSSHDevBox is a startable DevBox with SSH turned on for one key.
func newSSHDevBox(name, namespace string, keys ...string) *devpilotv1alpha1.DevBox {
	box := newTestDevBox(name, namespace)
	box.Spec.Access.SSH = &devpilotv1alpha1.SSHAccess{
		Enabled:        true,
		Username:       "root",
		AuthorizedKeys: keys,
	}
	return box
}

var _ = Describe("DevBox SSH hosting", func() {
	var (
		namespace string
		r         *DevBoxReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("ssh-%d-%d", GinkgoRandomSeed(), specCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
		r = newTestReconciler(newTestResolver())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	})

	// The pod mounts the secret, so it has to exist before the pod is created;
	// otherwise the container sits in ContainerCreating with a missing volume.
	It("creates the host key secret before the pod", func() {
		key := testClientKey()
		box := newSSHDevBox("ssh-first", namespace, key)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		boxKey := types.NamespacedName{Namespace: namespace, Name: box.Name}

		_, err := reconcileOnce(r, boxKey)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, boxKey)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + SSHSecretSuffix,
		}, secret)).To(Succeed())

		By("storing a host key the SSH stack can load")
		Expect(secret.Data).To(HaveKey(SSHHostKeyKey))
		Expect(secret.Data).To(HaveKey(SSHHostKeyPubKey))
		_, err = sshhost.ParseHostKey(secret.Data[SSHHostKeyKey])
		Expect(err).NotTo(HaveOccurred())

		By("listing exactly the requested key")
		Expect(string(secret.Data[SSHAuthorizedKeyID])).To(Equal(key + "\n"))

		By("mounting the secret read-only")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + PodSuffix,
		}, pod)).To(Succeed())
		Expect(pod.Spec.Volumes).To(ContainElement(Satisfy(func(v corev1.Volume) bool {
			return v.Name == SSHVolumeName && v.Secret != nil &&
				v.Secret.SecretName == box.Name+SSHSecretSuffix
		})))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(Satisfy(func(m corev1.VolumeMount) bool {
			return m.Name == SSHVolumeName && m.MountPath == SSHMountPath && m.ReadOnly
		})))

		By("telling devpilot-init to serve SSH")
		command := strings.Join(pod.Spec.Containers[0].Command, " ")
		Expect(command).To(ContainSubstring("--ssh-listen=:22"))
		Expect(command).To(ContainSubstring("--ssh-authorized-keys=" + SSHMountPath + "/" + SSHAuthorizedKeyID))
	})

	// Rotating the host key would invalidate every known_hosts entry the user
	// has accumulated, and the client has no out of band way to verify the new
	// one. Adding a key must therefore reuse the same host key.
	It("keeps the host key while authorized keys change", func() {
		first := testClientKey()
		box := newSSHDevBox("ssh-rotate", namespace, first)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		boxKey := types.NamespacedName{Namespace: namespace, Name: box.Name}

		_, err := reconcileOnce(r, boxKey)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, boxKey)
		Expect(err).NotTo(HaveOccurred())

		secretKey := types.NamespacedName{Namespace: namespace, Name: box.Name + SSHSecretSuffix}
		before := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, before)).To(Succeed())
		hostKey := before.Data[SSHHostKeyKey]

		second := testClientKey()
		Expect(k8sClient.Get(ctx, boxKey, box)).To(Succeed())
		box.Spec.Access.SSH.AuthorizedKeys = append(box.Spec.Access.SSH.AuthorizedKeys, second)
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		_, err = reconcileOnce(r, boxKey)
		Expect(err).NotTo(HaveOccurred())

		after := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, after)).To(Succeed())
		Expect(after.Data[SSHHostKeyKey]).To(Equal(hostKey))

		lines := strings.Split(strings.TrimSpace(string(after.Data[SSHAuthorizedKeyID])), "\n")
		Expect(lines).To(HaveLen(2))
		// The list is a set in the CRD, so the server may return it in any
		// order; comparing as a set keeps the assertion honest.
		Expect(slices.Contains(lines, first)).To(BeTrue())
		Expect(slices.Contains(lines, second)).To(BeTrue())
	})

	// A reconcile with nothing to change must not rewrite the secret: every
	// write wakes the informer and, for a mount, can restart a session.
	It("leaves the secret alone when nothing changed", func() {
		box := newSSHDevBox("ssh-stable", namespace, testClientKey())
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		secretKey := types.NamespacedName{Namespace: namespace, Name: box.Name + SSHSecretSuffix}

		_, err := r.ensureSSHSecret(ctx, box)
		Expect(err).NotTo(HaveOccurred())
		first := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, first)).To(Succeed())

		_, err = r.ensureSSHSecret(ctx, box)
		Expect(err).NotTo(HaveOccurred())
		second := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, second)).To(Succeed())

		Expect(second.ResourceVersion).To(Equal(first.ResourceVersion))
		Expect(bytes.Equal(second.Data[SSHAuthorizedKeyID], first.Data[SSHAuthorizedKeyID])).To(BeTrue())
	})

	It("refuses a secret whose host key was removed", func() {
		box := newSSHDevBox("ssh-broken", namespace, testClientKey())
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		secretKey := types.NamespacedName{Namespace: namespace, Name: box.Name + SSHSecretSuffix}
		_, err := r.ensureSSHSecret(ctx, box)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
		delete(secret.Data, SSHHostKeyKey)
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		// The error names the fix; regenerating silently would break every
		// known_hosts entry the user already trusts.
		_, err = r.ensureSSHSecret(ctx, box)
		Expect(err).To(MatchError(ContainSubstring("delete it to regenerate the host key")))
	})

	It("creates no secret when SSH is off", func() {
		box := newTestDevBox("ssh-off", namespace)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())

		_, err := reconcileOnce(r, types.NamespacedName{Namespace: namespace, Name: box.Name})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + SSHSecretSuffix,
		}, &corev1.Secret{})
		Expect(err).To(HaveOccurred())
	})
})
