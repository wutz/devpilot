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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// testIngressClass and testIngressDomain are the values the dev cluster's
	// controller is configured with.
	testIngressClass  = "nginx"
	testIngressDomain = "dev1.example.com"
	// testIngressPort is the port the dev cluster terminates TLS on.
	testIngressPort int32 = 7443

	// testUserTokenSecret is the secret a spec brings its own token in.
	testUserTokenSecret = "my-token"
	// testPortAPI and testPortInference are the two application ports the specs
	// publish.
	testPortAPI       = "api"
	testPortInference = "inference"
)

// newExposedReconciler is a reconciler that publishes HTTP endpoints. Every
// other spec runs with an unconfigured operator, so this is opt-in.
func newExposedReconciler() *DevBoxReconciler {
	r := newTestReconciler(newTestResolver())
	r.Config.IngressClass = testIngressClass
	r.Config.IngressDomain = testIngressDomain
	r.Config.IngressPort = testIngressPort
	return r
}

// newJupyterDevBox is a startable DevBox that hosts JupyterLab.
func newJupyterDevBox(name, namespace string) *devpilotv1alpha1.DevBox {
	box := newTestDevBox(name, namespace)
	box.Spec.Access.Jupyter = &devpilotv1alpha1.JupyterAccess{Enabled: true}
	return box
}

// httpConditionReason returns the reason of the HTTPAccessReady condition.
func httpConditionReason(box *devpilotv1alpha1.DevBox) string {
	for _, cond := range box.Status.Conditions {
		if cond.Type == ConditionHTTPAccessReady {
			return cond.Reason
		}
	}
	return ""
}

var _ = Describe("DevBox HTTP exposure", func() {
	var (
		namespace string
		r         *DevBoxReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("http-%d-%d", GinkgoRandomSeed(), specCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
		r = newExposedReconciler()
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	})

	// createBound creates a devbox and runs the reconciler far enough that the
	// claim is bound and the pod is built.
	createBound := func(box *devpilotv1alpha1.DevBox) types.NamespacedName {
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: box.Namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		return key
	}

	// markRunning simulates a kubelet that pulled the image and started the
	// container; the scheduled node name is set by the bz1-dev1 run.
	markRunning := func(box *devpilotv1alpha1.DevBox) {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: box.Namespace, Name: box.Name + PodSuffix,
		}, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("generates the token before the pod and mounts it read-only", func() {
		box := newJupyterDevBox("jupyter-token", namespace)
		createBound(box)

		By("storing a token long enough to be unguessable")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + JupyterSecretSuffix,
		}, secret)).To(Succeed())
		token := string(secret.Data[JupyterTokenKey])
		Expect(token).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(secret.OwnerReferences).To(HaveLen(1))
		Expect(secret.OwnerReferences[0].Name).To(Equal(box.Name))

		By("mounting exactly that token into the container")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + PodSuffix,
		}, pod)).To(Succeed())
		Expect(pod.Spec.Volumes).To(ContainElement(Satisfy(func(v corev1.Volume) bool {
			return v.Name == JupyterVolumeName && v.Secret != nil &&
				v.Secret.SecretName == box.Name+JupyterSecretSuffix &&
				v.Secret.Items[0].Key == JupyterTokenKey && v.Secret.Items[0].Path == JupyterTokenFile
		})))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(Satisfy(func(m corev1.VolumeMount) bool {
			return m.Name == JupyterVolumeName && m.MountPath == JupyterMountPath && m.ReadOnly
		})))

		By("telling devpilot-init to host JupyterLab behind it")
		command := strings.Join(pod.Spec.Containers[0].Command, " ")
		Expect(command).To(ContainSubstring("--jupyter-port=8888"))
		Expect(command).To(ContainSubstring("--jupyter-token-file=" + JupyterMountPath + "/" + JupyterTokenFile))
		Expect(pod.Spec.Containers[0].Ports).To(ContainElement(Satisfy(func(p corev1.ContainerPort) bool {
			return p.Name == JupyterServicePortName && p.ContainerPort == DefaultJupyterPort
		})))
	})

	// Rotating the token on every reconcile would log out a browser tab the
	// user already has open, and rewriting the secret wakes every mount.
	It("keeps the token while nothing asks for a new one", func() {
		box := newJupyterDevBox("jupyter-stable", namespace)
		key := createBound(box)

		secretKey := types.NamespacedName{Namespace: namespace, Name: box.Name + JupyterSecretSuffix}
		first := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, first)).To(Succeed())

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		second := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, second)).To(Succeed())
		Expect(second.ResourceVersion).To(Equal(first.ResourceVersion))
		Expect(second.Data[JupyterTokenKey]).To(Equal(first.Data[JupyterTokenKey]))
	})

	It("reuses a token the user supplied", func() {
		token := "user-chosen-token"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testUserTokenSecret, Namespace: namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"password": []byte(token)},
		})).To(Succeed())

		box := newJupyterDevBox("jupyter-user", namespace)
		box.Spec.Access.Jupyter.TokenSecretRef = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: testUserTokenSecret},
			Key:                  "password",
		}
		createBound(box)

		By("leaving the user's secret untouched")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: testUserTokenSecret}, secret)).To(Succeed())
		Expect(string(secret.Data["password"])).To(Equal(token))
		Expect(secret.OwnerReferences).To(BeEmpty())

		By("generating no secret of its own")
		err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + JupyterSecretSuffix,
		}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		By("mounting the user's secret instead")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + PodSuffix,
		}, pod)).To(Succeed())
		Expect(pod.Spec.Volumes).To(ContainElement(Satisfy(func(v corev1.Volume) bool {
			return v.Name == JupyterVolumeName && v.Secret != nil && v.Secret.SecretName == testUserTokenSecret
		})))
	})

	// The pod would sit in ContainerCreating with no useful message, so the
	// reconcile fails where the user can see why.
	It("fails when the referenced token secret is missing", func() {
		box := newJupyterDevBox("jupyter-nosecret", namespace)
		box.Spec.Access.Jupyter.TokenSecretRef = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "absent"},
			Key:                  "token",
		}
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})

		_, err = reconcileOnce(r, key)
		Expect(err).To(MatchError(ContainSubstring("get jupyter token secret absent")))
	})

	It("fails when the referenced secret has no token key", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "wrong-key", Namespace: namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"other": []byte("x")},
		})).To(Succeed())

		box := newJupyterDevBox("jupyter-badkey", namespace)
		box.Spec.Access.Jupyter.TokenSecretRef = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "wrong-key"},
			Key:                  "token",
		}
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})

		_, err = reconcileOnce(r, key)
		Expect(err).To(MatchError(ContainSubstring(`has no key "token"`)))
	})

	// One Service fronts every access path, so a gateway route and an Ingress
	// backend resolve the same name and the ports never drift apart.
	It("carries SSH, JupyterLab and the extra ports on one Service", func() {
		box := newSSHDevBox("http-ports", namespace, testClientKey())
		box.Spec.Access.Jupyter = &devpilotv1alpha1.JupyterAccess{Enabled: true}
		box.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{
			{Name: testPortAPI, Port: 3000, Protocol: ProtocolHTTP},
			{Name: "tensorboard", Port: 6006, Protocol: ProtocolHTTP},
		}
		createBound(box)

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, svc)).To(Succeed())
		Expect(svc.Spec.Selector).To(Equal(map[string]string{LabelDevBoxName: box.Name}))
		names := make([]string, 0, len(svc.Spec.Ports))
		for _, port := range svc.Spec.Ports {
			names = append(names, port.Name)
			Expect(port.TargetPort.IntVal).To(Equal(port.Port))
			Expect(port.Protocol).To(Equal(corev1.ProtocolTCP))
		}
		Expect(names).To(Equal([]string{SSHServicePortName, JupyterServicePortName, "api", "tensorboard"}))
	})

	It("publishes each HTTP port on its own host", func() {
		box := newJupyterDevBox("http-hosts", namespace)
		box.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{{Name: testPortAPI, Port: 3000, Protocol: ProtocolHTTP}}
		createBound(box)

		ingress := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, ingress)).To(Succeed())
		Expect(ingress.Labels).To(HaveKeyWithValue(LabelManagedBy, ValueManagedBy))
		Expect(ingress.Spec.IngressClassName).NotTo(BeNil())
		Expect(*ingress.Spec.IngressClassName).To(Equal(testIngressClass))

		By("naming the hosts in the TLS entry without pinning a certificate")
		Expect(ingress.Spec.TLS).To(HaveLen(1))
		Expect(ingress.Spec.TLS[0].SecretName).To(BeEmpty())
		Expect(ingress.Spec.TLS[0].Hosts).To(HaveLen(2))

		By("forwarding each host to its named Service port")
		Expect(ingress.Spec.Rules).To(HaveLen(2))
		want := map[string]string{
			"jupyter-" + box.Name + "-" + namespace:        JupyterServicePortName,
			testPortAPI + "-" + box.Name + "-" + namespace: "api",
		}
		for _, rule := range ingress.Spec.Rules {
			label, ok := strings.CutSuffix(rule.Host, "."+testIngressDomain)
			Expect(ok).To(BeTrue(), "host %q is not under %q", rule.Host, testIngressDomain)
			portName, known := want[label]
			Expect(known).To(BeTrue(), "unexpected host %q", rule.Host)
			Expect(rule.HTTP.Paths).To(HaveLen(1))
			Expect(rule.HTTP.Paths[0].Path).To(Equal("/"))
			Expect(*rule.HTTP.Paths[0].PathType).To(Equal(networkingv1.PathTypePrefix))
			Expect(rule.HTTP.Paths[0].Backend.Service.Name).To(Equal(box.Name))
			Expect(rule.HTTP.Paths[0].Backend.Service.Port.Name).To(Equal(portName))
		}

		By("raising the proxy limits a notebook needs")
		Expect(ingress.Annotations).To(HaveKeyWithValue("nginx.ingress.kubernetes.io/proxy-body-size", "0"))
		Expect(ingress.Annotations).To(HaveKeyWithValue("nginx.ingress.kubernetes.io/proxy-read-timeout", "3600"))
		Expect(ingress.Annotations).To(HaveKeyWithValue("nginx.ingress.kubernetes.io/proxy-buffering", "off"))
		Expect(ingress.Annotations).NotTo(HaveKey("nginx.ingress.kubernetes.io/backend-protocol"))
	})

	// The backend protocol is per Ingress, so a gRPC port cannot share the one
	// that carries HTTP.
	It("splits gRPC ports into their own Ingress", func() {
		box := newTestDevBox("http-grpc", namespace)
		box.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{
			{Name: testPortAPI, Port: 3000, Protocol: ProtocolHTTP},
			{Name: testPortInference, Port: 50051, Protocol: ProtocolGRPC},
		}
		createBound(box)

		grpc := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + IngressGRPCSuffix,
		}, grpc)).To(Succeed())
		Expect(grpc.Annotations).To(HaveKeyWithValue("nginx.ingress.kubernetes.io/backend-protocol", "GRPC"))
		Expect(grpc.Spec.Rules).To(HaveLen(1))
		Expect(grpc.Spec.Rules[0].Host).
			To(Equal(testPortInference + "-" + box.Name + "-" + namespace + "." + testIngressDomain))
		Expect(grpc.Spec.TLS[0].Hosts).To(ConsistOf(grpc.Spec.Rules[0].Host))

		httpIngress := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, httpIngress)).To(Succeed())
		Expect(httpIngress.Spec.Rules).To(HaveLen(1))
		Expect(httpIngress.Spec.Rules[0].Host).
			To(Equal(testPortAPI + "-" + box.Name + "-" + namespace + "." + testIngressDomain))
	})

	It("reports the public URLs while the devbox runs", func() {
		box := newJupyterDevBox("http-endpoints", namespace)
		box.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{
			{Name: testPortAPI, Port: 3000, Protocol: ProtocolHTTP},
			{Name: testPortInference, Port: 50051, Protocol: ProtocolGRPC},
		}
		key := createBound(box)
		markRunning(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(stored.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
		prefix := func(port string) string {
			return fmt.Sprintf("https://%s-%s-%s.%s:%d/", port, box.Name, namespace, testIngressDomain, testIngressPort)
		}
		Expect(stored.Status.Endpoints.Jupyter).To(Equal(prefix(JupyterServicePortName)))
		Expect(stored.Status.Endpoints.Ports).To(Equal(map[string]string{
			testPortAPI:       prefix(testPortAPI),
			testPortInference: strings.Replace(prefix(testPortInference), "https://", "grpcs://", 1),
		}))
		Expect(metaConditionStatus(stored, ConditionHTTPAccessReady)).To(Equal(metav1.ConditionTrue))
		Expect(httpConditionReason(stored)).To(Equal("IngressReady"))
	})

	It("omits the port when the controller serves 443", func() {
		r.Config.IngressPort = DefaultIngressPort
		box := newJupyterDevBox("http-443", namespace)
		key := createBound(box)
		markRunning(box)

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(stored.Status.Endpoints.Jupyter).
			To(Equal("https://jupyter-" + box.Name + "-" + namespace + "." + testIngressDomain + "/"))
	})

	// Without a domain and a class there is no host to hand out; publishing a
	// URL nothing serves would be worse than saying so.
	It("says why it cannot publish when the operator is unconfigured", func() {
		r = newTestReconciler(newTestResolver())
		box := newJupyterDevBox("http-unconfigured", namespace)
		key := createBound(box)

		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, &networkingv1.Ingress{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(metaConditionStatus(stored, ConditionHTTPAccessReady)).To(Equal(metav1.ConditionFalse))
		Expect(httpConditionReason(stored)).To(Equal("NoIngressDomain"))
	})

	It("withdraws the Ingress when the ports are removed", func() {
		box := newJupyterDevBox("http-withdraw", namespace)
		box.Spec.Access.Ports = []devpilotv1alpha1.DevBoxPort{{Name: testPortAPI, Port: 3000, Protocol: ProtocolHTTP}}
		key := createBound(box)

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		stored.Spec.Access.Ports = nil
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		// The devbox still hosts JupyterLab, so its host must survive.
		ingress := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, ingress)).To(Succeed())
		Expect(ingress.Spec.Rules).To(HaveLen(1))
		Expect(ingress.Spec.Rules[0].Host).To(Equal("jupyter-" + box.Name + "-" + namespace + "." + testIngressDomain))

		By("dropping the whole Ingress once nothing HTTP is left")
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		stored.Spec.Access.Jupyter = nil
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, &networkingv1.Ingress{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Ingress should be gone, got %v", err)
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(httpConditionReason(stored)).To(BeEmpty(), "a spec with no HTTP left must not keep the verdict")
	})

	// The port is a flag devpilot-init reads once, so it replaces the pod; the
	// token is read from the mounted secret, so a new one must not.
	It("replaces the pod when the Jupyter port changes only", func() {
		box := newJupyterDevBox("http-replace", namespace)
		key := createBound(box)

		podKey := types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}
		before := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey, before)).To(Succeed())

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		stored.Spec.Access.Jupyter.Port = 9999
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		after := &corev1.Pod{}
		err = k8sClient.Get(ctx, podKey, after)
		Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		if err == nil {
			Expect(podHash(after)).NotTo(Equal(podHash(before)))
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})

	It("leaves the pod alone when only the token changes", func() {
		box := newJupyterDevBox("http-token-change", namespace)
		key := createBound(box)

		podKey := types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}
		before := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey, before)).To(Succeed())
		hash := podHash(before)

		By("deleting the secret, which is the only way to rotate")
		stale := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: box.Name + JupyterSecretSuffix},
		}
		Expect(k8sClient.Delete(ctx, stale)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + JupyterSecretSuffix,
		}, secret)).To(Succeed())
		Expect(string(secret.Data[JupyterTokenKey])).To(MatchRegexp(`^[0-9a-f]{64}$`))

		after := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey, after)).To(Succeed())
		Expect(podHash(after)).To(Equal(hash), "a rotated token must not replace the pod")
	})
})

var _ = Describe("HTTP hostnames", func() {
	It("folds a name into a DNS label", func() {
		Expect(dnsLabel("Jupyter-Dev_Box.Proj")).To(Equal("jupyter-dev-box-proj"))
		Expect(dnsLabel("--dev--")).To(Equal("dev"))
		Expect(dnsLabel("")).To(Equal("devbox"))
		Expect(dnsLabel("123")).To(Equal("123"))
	})

	// A host label is capped at 63 characters, and two long names sharing a
	// prefix must not land on one host.
	It("hash-suffixes a label that does not fit", func() {
		long := strings.Repeat("x", 80)
		label := dnsLabel(long)
		Expect(len(label)).To(BeNumerically("<=", 63))
		Expect(label).To(MatchRegexp(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`))
		Expect(dnsLabel(long + "y")).NotTo(Equal(label))
		Expect(dnsLabel(long)).To(Equal(label), "the host must be stable")
	})
})
