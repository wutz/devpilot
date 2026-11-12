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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sshgatewayv1alpha1 "github.com/wutz/sshgateway/api/v1alpha1"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// testGatewayName is the gateway every spec publishes on.
	testGatewayName = "gw"
	// testGatewayController is a stand-in for the controller name the gateway
	// operator writes into a route's parent status.
	testGatewayController = "sshgateway.wutz.dev/sshpiper"
	// testSSHLoginName is the account the built-in server reports.
	testSSHLoginName = "root"
)

// routeConditionReason returns the reason of the SSHRouteReady condition, or ""
// when the devbox has none.
func routeConditionReason(box *devpilotv1alpha1.DevBox) string {
	for _, cond := range box.Status.Conditions {
		if cond.Type == ConditionSSHRouteReady {
			return cond.Reason
		}
	}
	return ""
}

// gatewayRef points a devbox at the gateway in namespace. It is always explicit
// because the gateway lives outside the devbox's namespace, and every spec gets
// its own gateway namespace so one spec's identity secret cannot leak into the
// next.
func gatewayRef(namespace string) *devpilotv1alpha1.NamespacedName {
	return &devpilotv1alpha1.NamespacedName{Name: testGatewayName, Namespace: namespace}
}

// seedGatewayIdentity writes the public key the gateway operator would create
// on first use. The devbox lists it so the gateway may log in.
func seedGatewayIdentity(namespace, publicKey string) {
	ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: GatewayIdentitySecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{GatewayIdentityPublicKeyKey: []byte(publicKey + "\n")},
	}))).To(Succeed())
}

var _ = Describe("DevBox gateway publishing", func() {
	var (
		namespace   string
		gwNamespace string
		r           *DevBoxReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("sshroute-%d-%d", GinkgoRandomSeed(), specCounter)
		gwNamespace = namespace + "-gw"
		for _, name := range []string{namespace, gwNamespace} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: name},
			})).To(Succeed())
		}
		r = newTestReconciler(newTestResolver())
	})

	AfterEach(func() {
		for _, name := range []string{namespace, gwNamespace} {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
		}
	})

	// createBound creates a devbox and runs the reconciler far enough that the
	// claim is bound and the pod build starts, so the Service and the route have
	// been reconciled.
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

	It("publishes the devbox on its gateway", func() {
		clientKey := testClientKey()
		box := newSSHDevBox("route-dev", namespace, clientKey)
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		createBound(box)

		By("creating a Service the gateway can resolve the backend through")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, svc)).To(Succeed())
		Expect(svc.Labels).To(HaveKeyWithValue(LabelManagedBy, ValueManagedBy))
		Expect(svc.Spec.Selector).To(Equal(map[string]string{LabelDevBoxName: box.Name}))
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Name).To(Equal(SSHServicePortName))
		Expect(svc.Spec.Ports[0].Port).To(Equal(SSHPort))
		Expect(svc.Spec.Ports[0].TargetPort.IntVal).To(Equal(SSHPort))
		Expect(svc.OwnerReferences).To(HaveLen(1))
		Expect(svc.OwnerReferences[0].Name).To(Equal(box.Name))

		By("creating the route that maps a gateway username to that Service")
		route := &sshgatewayv1alpha1.SSHRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, route)).To(Succeed())
		Expect(route.Spec.ParentRefs).To(HaveLen(1))
		Expect(route.Spec.ParentRefs[0].Name).To(Equal(testGatewayName))
		Expect(route.Spec.ParentRefs[0].Namespace).NotTo(BeNil())
		Expect(*route.Spec.ParentRefs[0].Namespace).To(Equal(gwNamespace))
		Expect(route.Spec.Rules).To(HaveLen(1))
		rule := route.Spec.Rules[0]
		Expect(rule.Matches).To(HaveLen(1))
		Expect(rule.Matches[0].Username).To(Equal(sanitizeUsername(namespace + "-" + box.Name)))
		Expect(rule.AuthorizedKeys).To(Equal([]string{clientKey}))
		Expect(rule.BackendRefs).To(HaveLen(1))
		Expect(rule.BackendRefs[0].Name).To(Equal(box.Name))
		Expect(rule.BackendRefs[0].Port).To(Equal(SSHPort))
		Expect(rule.BackendRefs[0].Username).To(Equal(testSSHLoginName))
		Expect(route.OwnerReferences).To(HaveLen(1))
	})

	// The gateway logs in to the backend with the platform identity, whose key
	// the gateway operator creates on its own schedule. Without that key in
	// authorized_keys the route resolves and the login is still refused.
	It("authorizes the gateway identity key on the devbox", func() {
		seedGatewayIdentity(gwNamespace, testClientKey())
		box := newSSHDevBox("route-identity", namespace, testClientKey())
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		createBound(box)

		identity := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: gwNamespace, Name: GatewayIdentitySecretName,
		}, identity)).To(Succeed())
		gatewayKey := strings.TrimSpace(string(identity.Data[GatewayIdentityPublicKeyKey]))

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + SSHSecretSuffix,
		}, secret)).To(Succeed())
		lines := strings.Split(strings.TrimSpace(string(secret.Data[SSHAuthorizedKeyID])), "\n")
		Expect(lines).To(ContainElement(gatewayKey))
		Expect(lines).To(ContainElement(box.Spec.Access.SSH.AuthorizedKeys[0]))
		Expect(lines).To(HaveLen(2))

		By("leaving the route's keys to the user's clients only")
		route := &sshgatewayv1alpha1.SSHRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, route)).To(Succeed())
		Expect(route.Spec.Rules[0].AuthorizedKeys).To(Equal(box.Spec.Access.SSH.AuthorizedKeys))
	})

	It("deduplicates a gateway key that is already a client key", func() {
		shared := testClientKey()
		seedGatewayIdentity(gwNamespace, shared)
		box := newSSHDevBox("route-shared", namespace, shared)
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		createBound(box)

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: box.Name + SSHSecretSuffix,
		}, secret)).To(Succeed())
		Expect(string(secret.Data[SSHAuthorizedKeyID])).To(Equal(shared + "\n"))
	})

	It("creates no route when there is no key to log in with", func() {
		box := newSSHDevBox("route-nokeys", namespace)
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		key := createBound(box)

		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, &sshgatewayv1alpha1.SSHRoute{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		By("saying why in the condition")
		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(metaConditionStatus(stored, ConditionSSHRouteReady)).To(Equal(metav1.ConditionFalse))
		Expect(routeConditionReason(stored)).To(Equal("NoAuthorizedKeys"))
	})

	It("withdraws the route and the Service when the gatewayRef is removed", func() {
		box := newSSHDevBox("route-withdraw", namespace, testClientKey())
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		key := createBound(box)

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		stored.Spec.Access.SSH.GatewayRef = nil
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name},
			&sshgatewayv1alpha1.SSHRoute{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the SSHRoute should be gone, got %v", err)
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, &corev1.Service{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Service should be gone, got %v", err)
	})

	// A Service the operator did not create is the user's; withdrawing the
	// gateway must not take their traffic with it.
	It("leaves a Service it did not create alone", func() {
		box := newSSHDevBox("route-usersvc", namespace, testClientKey())
		Expect(k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: namespace},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Name: "custom", Port: 8080}},
			},
		})).To(Succeed())

		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		key := createBound(box)

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		stored.Spec.Access.SSH.GatewayRef = nil
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, svc)).To(Succeed())
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Name).To(Equal("custom"))
	})

	It("reports the route as pending until a gateway comments on it", func() {
		box := newSSHDevBox("route-noapi", namespace, testClientKey())
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		key := createBound(box)

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(metaConditionStatus(stored, ConditionSSHRouteReady)).To(Equal(metav1.ConditionFalse))

		// The route exists here (its CRD is installed), so the condition must be
		// the honest "not reported yet" rather than either of the ready states.
		Expect([]string{"RoutePending", "RouteNotAccepted"}).
			To(ContainElement(routeConditionReason(stored)))
	})

	It("reports the route accepted once the gateway says so", func() {
		box := newSSHDevBox("route-accepted", namespace, testClientKey())
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		key := createBound(box)

		route := &sshgatewayv1alpha1.SSHRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, route)).To(Succeed())
		reason := sshgatewayv1alpha1.SSHReasonAccepted
		route.Status.Parents = []sshgatewayv1alpha1.RouteParentStatus{{
			ParentRef:      route.Spec.ParentRefs[0],
			ControllerName: testGatewayController,
			Conditions: []metav1.Condition{
				{Type: sshgatewayv1alpha1.SSHConditionAccepted, Status: metav1.ConditionTrue,
					Reason: reason, LastTransitionTime: metav1.Now()},
				{Type: sshgatewayv1alpha1.SSHConditionResolvedRefs, Status: metav1.ConditionTrue,
					Reason: reason, LastTransitionTime: metav1.Now()},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(metaConditionStatus(stored, ConditionSSHRouteReady)).To(Equal(metav1.ConditionTrue))
		Expect(routeConditionReason(stored)).To(Equal("RouteAccepted"))
	})

	It("reports a backend the gateway could not resolve", func() {
		box := newSSHDevBox("route-unresolved", namespace, testClientKey())
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		key := createBound(box)

		route := &sshgatewayv1alpha1.SSHRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, route)).To(Succeed())
		route.Status.Parents = []sshgatewayv1alpha1.RouteParentStatus{{
			ParentRef:      route.Spec.ParentRefs[0],
			ControllerName: testGatewayController,
			Conditions: []metav1.Condition{
				{Type: sshgatewayv1alpha1.SSHConditionAccepted, Status: metav1.ConditionTrue,
					Reason: sshgatewayv1alpha1.SSHReasonAccepted, LastTransitionTime: metav1.Now()},
				{Type: sshgatewayv1alpha1.SSHConditionResolvedRefs, Status: metav1.ConditionFalse,
					Reason:             sshgatewayv1alpha1.SSHReasonHostKeyNotFound,
					Message:            "secret route-unresolved-hostkey has no ssh_host_ed25519_key",
					LastTransitionTime: metav1.Now()},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(metaConditionStatus(stored, ConditionSSHRouteReady)).To(Equal(metav1.ConditionFalse))
		Expect(routeConditionReason(stored)).
			To(Equal(sshgatewayv1alpha1.SSHReasonHostKeyNotFound))
	})

	// The pod endpoint dies with the pod; the gateway endpoint survives a stop and
	// is the one worth writing into an SSH config.
	It("reports the gateway endpoint while the route resolves", func() {
		box := newSSHDevBox("route-endpoint", namespace, testClientKey())
		box.Spec.Access.SSH.GatewayRef = gatewayRef(gwNamespace)
		key := createBound(box)

		gateway := &sshgatewayv1alpha1.SSHGateway{
			ObjectMeta: metav1.ObjectMeta{Name: testGatewayName, Namespace: gwNamespace},
			Spec: sshgatewayv1alpha1.SSHGatewaySpec{
				GatewayClassName: "sshpiper",
				Listeners:        []sshgatewayv1alpha1.SSHListener{{Name: "ssh", Port: 2222}},
			},
		}
		Expect(k8sClient.Create(ctx, gateway)).To(Succeed())
		gateway.Status.Addresses = []string{"203.0.113.10:2222"}
		Expect(k8sClient.Status().Update(ctx, gateway)).To(Succeed())

		route := &sshgatewayv1alpha1.SSHRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name}, route)).To(Succeed())
		route.Status.Parents = []sshgatewayv1alpha1.RouteParentStatus{{
			ParentRef:      route.Spec.ParentRefs[0],
			ControllerName: testGatewayController,
			Conditions: []metav1.Condition{
				{Type: sshgatewayv1alpha1.SSHConditionAccepted, Status: metav1.ConditionTrue,
					Reason: sshgatewayv1alpha1.SSHReasonAccepted, LastTransitionTime: metav1.Now()},
				{Type: sshgatewayv1alpha1.SSHConditionResolvedRefs, Status: metav1.ConditionTrue,
					Reason: sshgatewayv1alpha1.SSHReasonAccepted, LastTransitionTime: metav1.Now()},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())

		stored := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		// The namespace-qualified name overflows 32 characters here, so this also
		// covers the hash-suffixed form a client would put in its SSH config.
		want := sshRouteUsername(stored) + "@203.0.113.10:2222"
		Expect(r.sshGatewayEndpoint(ctx, stored)).To(Equal(want))
	})
})

var _ = Describe("Gateway usernames", func() {
	DescribeTable("sanitizeUsername folds a name into the route pattern",
		func(raw, want string) {
			Expect(sanitizeUsername(raw)).To(Equal(want))
		},
		Entry("keeps a plain name", "devbox-a1", "devbox-a1"),
		Entry("lowercases", "DevBox-A1", "devbox-a1"),
		Entry("folds dots and slashes", "team.dev/proj", "team-dev-proj"),
		Entry("trims leading separators", "--team--", "team"),
		Entry("prefixes a leading digit", "1devbox", "devbox-1devbox"),
		Entry("trims a leading underscore with the other separators", "_devbox", "devbox"),
		Entry("falls back for an empty name", "", "devbox"),
		Entry("falls back for separators only", "---", "devbox"),
	)

	It("keeps the readable form when it fits", func() {
		box := newTestDevBox("dev", "proj")
		box.Spec.Access = devpilotv1alpha1.DevBoxAccess{
			SSH: &devpilotv1alpha1.SSHAccess{Enabled: true, Username: testSSHLoginName},
		}
		Expect(sshRouteUsername(box)).To(Equal("proj-dev"))
	})

	It("hash-suffixes a name that does not fit", func() {
		box := newTestDevBox(strings.Repeat("x", 40), "proj")
		box.Spec.Access = devpilotv1alpha1.DevBoxAccess{
			SSH: &devpilotv1alpha1.SSHAccess{Enabled: true, Username: testSSHLoginName},
		}
		name := sshRouteUsername(box)
		Expect(len(name)).To(BeNumerically("<=", maxSSHUsernameLen))
		Expect(name).To(MatchRegexp(`^[a-z_][a-z0-9_-]{0,31}$`))
		// The hash is what tells two names that share a long prefix apart.
		other := newTestDevBox(strings.Repeat("x", 39)+"y", "proj")
		Expect(sshRouteUsername(other)).NotTo(Equal(name))
	})

	It("keeps the name stable across calls", func() {
		box := newTestDevBox("dev", "proj")
		Expect(sshRouteUsername(box)).To(Equal(sshRouteUsername(box)))
	})

	It("logs the gateway in as root unless the account name fits", func() {
		box := newTestDevBox("dev", "proj")
		box.Spec.Access = devpilotv1alpha1.DevBoxAccess{
			SSH: &devpilotv1alpha1.SSHAccess{Enabled: true, Username: "dev-user"},
		}
		Expect(sshBackendUsername(box)).To(Equal("dev-user"))

		box.Spec.Access.SSH.Username = "Dev.User"
		Expect(sshBackendUsername(box)).To(Equal("root"))

		box.Spec.Access.SSH.Username = ""
		Expect(sshBackendUsername(box)).To(Equal("root"))
	})
})
