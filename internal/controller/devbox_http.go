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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// JupyterServicePortName is the Service port that fronts the hosted
	// JupyterLab.
	JupyterServicePortName = "jupyter"
	// DefaultJupyterPort is JupyterLab's own default port.
	DefaultJupyterPort int32 = 8888
	// JupyterSecretSuffix is appended to the DevBox name for the token secret
	// the operator generates when spec.access.jupyter.tokenSecretRef is empty.
	JupyterSecretSuffix = "-jupyter"
	// JupyterTokenKey is the data key inside that secret.
	JupyterTokenKey = "token"
	// JupyterVolumeName is the pod volume that exposes the token.
	JupyterVolumeName = "jupyter-token"
	// JupyterTokenFile is the file name inside that volume. The path must match
	// initroot.JupyterDir, which sits outside /run/devpilot so the submount
	// migration carries it into the pivoted root.
	JupyterMountPath = "/run/devpilot-jupyter"
	// JupyterTokenFile is the file devpilot-init reads the token from.
	JupyterTokenFile = "token"

	// IngressGRPCSuffix names the second Ingress that carries gRPC hosts. The
	// backend protocol annotation is per Ingress, so HTTP and gRPC hosts cannot
	// share one.
	IngressGRPCSuffix = "-grpc"
	// ConditionHTTPAccessReady tracks publishing the HTTP endpoints.
	ConditionHTTPAccessReady = "HTTPAccessReady"

	// ProtocolHTTP and ProtocolGRPC mirror the DevBoxPort enum.
	ProtocolHTTP = "HTTP"
	ProtocolGRPC = "GRPC"

	// maxDNSLabelLen is the DNS-1123 ceiling for a host's leftmost label.
	maxDNSLabelLen = 63
	// hostHashLen is how many hex characters a truncated label keeps.
	hostHashLen = 8
)

// jupyterEnabled reports whether the pod should host JupyterLab.
func jupyterEnabled(box *devpilotv1alpha1.DevBox) bool {
	return box.Spec.Access.Jupyter != nil && box.Spec.Access.Jupyter.Enabled
}

// jupyterPort is the port JupyterLab listens on, defaulted.
func jupyterPort(box *devpilotv1alpha1.DevBox) int32 {
	if jupyterEnabled(box) && box.Spec.Access.Jupyter.Port != 0 {
		return box.Spec.Access.Jupyter.Port
	}
	return DefaultJupyterPort
}

// jupyterTokenRef resolves where the token comes from: the user's own secret
// when spec.access.jupyter.tokenSecretRef names one, the generated secret
// otherwise. The second result says which of the two it was.
func jupyterTokenRef(box *devpilotv1alpha1.DevBox) (corev1.SecretKeySelector, bool) {
	if jupyterEnabled(box) && box.Spec.Access.Jupyter.TokenSecretRef != nil {
		return *box.Spec.Access.Jupyter.TokenSecretRef, false
	}
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: box.Name + JupyterSecretSuffix},
		Key:                  JupyterTokenKey,
	}, true
}

// ensureJupyterSecret materializes the token the hosted JupyterLab authenticates
// with. A user supplied secret is only checked, never rewritten: the token is
// theirs.
func (r *DevBoxReconciler) ensureJupyterSecret(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	if !jupyterEnabled(box) {
		return nil
	}
	log := logf.FromContext(ctx)
	ref, managed := jupyterTokenRef(box)

	if !managed {
		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: box.Namespace, Name: ref.Name}
		if err := r.Get(ctx, key, secret); err != nil {
			return fmt.Errorf("get jupyter token secret %s: %w", key.Name, err)
		}
		if _, ok := secret.Data[ref.Key]; !ok {
			// The pod would sit in ContainerCreating with no useful message, so
			// this fails the reconcile instead, where the user can see it.
			return fmt.Errorf("jupyter token secret %s has no key %q", key.Name, ref.Key)
		}
		return nil
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: box.Namespace, Name: ref.Name}
	err := r.Get(ctx, key, secret)
	if isNotFound(err) {
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return fmt.Errorf("generate jupyter token: %w", err)
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      key.Name,
				Namespace: key.Namespace,
				Labels:    map[string]string{LabelManagedBy: ValueManagedBy, LabelDevBoxName: box.Name},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{JupyterTokenKey: []byte(hex.EncodeToString(token))},
		}
		if ownerErr := ctrlSetOwner(secret, box, r.Scheme); ownerErr != nil {
			return fmt.Errorf("set owner reference on secret %s: %w", key.Name, ownerErr)
		}
		if createErr := r.Create(ctx, secret); createErr != nil && !isAlreadyExists(createErr) {
			return fmt.Errorf("create jupyter token secret %s: %w", key.Name, createErr)
		}
		log.Info("Created Jupyter token", "secret", key.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get jupyter token secret %s: %w", key.Name, err)
	}
	if _, ok := secret.Data[JupyterTokenKey]; !ok {
		// Rotating would lock out a browser tab the user already has open, and
		// deleting the secret is the deliberate way to rotate.
		return fmt.Errorf("secret %s has no %s; delete it to issue a new token", key.Name, JupyterTokenKey)
	}
	return nil
}

// jupyterVolume exposes the token to the main container, read-only.
func jupyterVolume(box *devpilotv1alpha1.DevBox) corev1.Volume {
	ref, _ := jupyterTokenRef(box)
	return corev1.Volume{
		Name: JupyterVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				DefaultMode: new(int32(0o400)),
				Items:       []corev1.KeyToPath{{Key: ref.Key, Path: JupyterTokenFile}},
			},
		},
	}
}

// jupyterCommandFlags tell devpilot-init to host JupyterLab behind the token.
func jupyterCommandFlags(box *devpilotv1alpha1.DevBox) []string {
	return []string{
		fmt.Sprintf("--jupyter-port=%d", jupyterPort(box)),
		"--jupyter-token-file=" + JupyterMountPath + "/" + JupyterTokenFile,
	}
}

// accessServicePorts lists every port the access Service carries: SSH, the
// hosted JupyterLab and the user's extra ports, in that order. The Service is
// shared so one Ingress backend and one gateway backend resolve the same name.
func accessServicePorts(box *devpilotv1alpha1.DevBox) []corev1.ServicePort {
	ports := make([]corev1.ServicePort, 0, 2+len(box.Spec.Access.Ports))
	if sshEnabled(box) {
		ports = append(ports, corev1.ServicePort{
			Name:       SSHServicePortName,
			Port:       SSHPort,
			TargetPort: intstr.FromInt32(SSHPort),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	if jupyterEnabled(box) {
		port := jupyterPort(box)
		ports = append(ports, corev1.ServicePort{
			Name:       JupyterServicePortName,
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	for _, extra := range box.Spec.Access.Ports {
		ports = append(ports, corev1.ServicePort{
			Name:       extra.Name,
			Port:       extra.Port,
			TargetPort: intstr.FromInt32(extra.Port),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}

// accessService is the Service that fronts every access path into the devbox.
func accessService(box *devpilotv1alpha1.DevBox) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      box.Name,
			Namespace: box.Namespace,
			Labels:    map[string]string{LabelManagedBy: ValueManagedBy, LabelDevBoxName: box.Name},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{LabelDevBoxName: box.Name},
			Ports:    accessServicePorts(box),
		},
	}
}

// ensureAccessService keeps the shared access Service in step with the spec. It
// selects the pod by the DevBox label, so it follows the pod across
// replacements, and it outlives a stop so a published hostname or gateway
// username stays valid.
func (r *DevBoxReconciler) ensureAccessService(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	log := logf.FromContext(ctx)

	if len(accessServicePorts(box)) == 0 {
		return r.withdrawAccessService(ctx, box)
	}

	wanted := accessService(box)
	existing := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKeyFromObject(wanted), existing)
	if isNotFound(err) {
		if ownerErr := ctrlSetOwner(wanted, box, r.Scheme); ownerErr != nil {
			return fmt.Errorf("set owner reference on service %s: %w", wanted.Name, ownerErr)
		}
		if createErr := r.Create(ctx, wanted); createErr != nil && !isAlreadyExists(createErr) {
			return fmt.Errorf("create service %s: %w", wanted.Name, createErr)
		}
		log.Info("Created access service", "service", wanted.Name, "ports", len(wanted.Spec.Ports))
		return nil
	}
	if err != nil {
		return fmt.Errorf("get service %s: %w", wanted.Name, err)
	}
	if existing.Labels[LabelManagedBy] != ValueManagedBy {
		// A Service the operator did not create is the user's: leaving it alone is
		// the only safe answer, and the gateway reports the missing host key on
		// the route if it does not match.
		return nil
	}
	if equality.Semantic.DeepEqual(existing.Spec.Selector, wanted.Spec.Selector) &&
		equality.Semantic.DeepEqual(existing.Spec.Ports, wanted.Spec.Ports) {
		return nil
	}
	existing.Spec.Selector = wanted.Spec.Selector
	existing.Spec.Ports = wanted.Spec.Ports
	if updateErr := r.Update(ctx, existing); updateErr != nil {
		return fmt.Errorf("update service %s: %w", wanted.Name, updateErr)
	}
	log.Info("Updated access service", "service", wanted.Name, "ports", len(wanted.Spec.Ports))
	return nil
}

// withdrawAccessService removes the operator's Service once no access path
// needs it any more.
func (r *DevBoxReconciler) withdrawAccessService(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	log := logf.FromContext(ctx)
	key := client.ObjectKey{Namespace: box.Namespace, Name: box.Name}

	svc := &corev1.Service{}
	switch err := r.Get(ctx, key, svc); {
	case err == nil:
		// Only the operator's own Service is removed: one the user created under
		// the same name is theirs, and deleting it would take their traffic.
		if svc.Labels[LabelManagedBy] != ValueManagedBy {
			return nil
		}
		if delErr := r.Delete(ctx, svc); delErr != nil && !isNotFound(delErr) {
			return fmt.Errorf("delete service %s: %w", svc.Name, delErr)
		}
		log.Info("Deleted access service", "service", svc.Name)
	case isNotFound(err):
	default:
		return fmt.Errorf("get service %s: %w", key.Name, err)
	}
	return nil
}

// httpPorts returns the spec ports the operator publishes over HTTP.
func httpPorts(box *devpilotv1alpha1.DevBox) []devpilotv1alpha1.DevBoxPort {
	out := make([]devpilotv1alpha1.DevBoxPort, 0, len(box.Spec.Access.Ports))
	out = append(out, box.Spec.Access.Ports...)
	return out
}

// ensureHTTPAccess publishes the devbox's HTTP endpoints behind the cluster
// ingress, or withdraws them when the spec asks for none.
func (r *DevBoxReconciler) ensureHTTPAccess(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	httpIngress, grpcIngress := r.httpIngresses(box)
	wanted := map[string]*networkingv1.Ingress{}
	for _, ingress := range []*networkingv1.Ingress{httpIngress, grpcIngress} {
		if ingress != nil {
			wanted[ingress.Name] = ingress
		}
	}

	for _, name := range []string{box.Name, box.Name + IngressGRPCSuffix} {
		if ingress, ok := wanted[name]; ok {
			if err := r.ensureIngress(ctx, box, ingress); err != nil {
				return err
			}
			continue
		}
		if err := r.withdrawIngress(ctx, box, name); err != nil {
			return err
		}
	}

	r.observeHTTPAccess(box, httpIngress, grpcIngress)
	return nil
}

// observeHTTPAccess records whether the HTTP endpoints were published and, when
// they were not, why.
func (r *DevBoxReconciler) observeHTTPAccess(
	box *devpilotv1alpha1.DevBox, httpIngress, grpcIngress *networkingv1.Ingress,
) {
	if !jupyterEnabled(box) && len(httpPorts(box)) == 0 {
		// Nothing asked for HTTP, so a stale condition would only confuse.
		r.clearCondition(box, ConditionHTTPAccessReady)
		return
	}
	if httpIngress == nil && grpcIngress == nil {
		r.setCondition(box, ConditionHTTPAccessReady, metav1ConditionFalse, "NoIngressDomain",
			"the operator has no --ingress-domain configured, so the devbox has no public URL")
		return
	}
	hosts := ingressHosts(httpIngress, grpcIngress)
	r.setCondition(box, ConditionHTTPAccessReady, metav1ConditionTrue, "IngressReady",
		"Published on "+strings.Join(hosts, ", "))
}

// ingressHosts flattens the hosts of the given ingresses.
func ingressHosts(ingresses ...*networkingv1.Ingress) []string {
	var hosts []string
	for _, ingress := range ingresses {
		if ingress == nil {
			continue
		}
		for _, rule := range ingress.Spec.Rules {
			hosts = append(hosts, rule.Host)
		}
	}
	return hosts
}

// httpIngresses builds the Ingresses the spec asks for, or nils when the
// operator is not configured to publish any.
func (r *DevBoxReconciler) httpIngresses(
	box *devpilotv1alpha1.DevBox,
) (httpIngress, grpcIngress *networkingv1.Ingress) {
	if !r.Config.HTTPExposed() {
		return nil, nil
	}

	var httpRules, grpcRules []networkingv1.IngressRule
	var httpHosts, grpcHosts []string
	if jupyterEnabled(box) {
		host := r.httpHost(box, JupyterServicePortName)
		httpHosts = append(httpHosts, host)
		httpRules = append(httpRules, ingressRule(box.Name, host, JupyterServicePortName))
	}
	for _, extra := range httpPorts(box) {
		host := r.httpHost(box, extra.Name)
		if extra.Protocol == ProtocolGRPC {
			grpcHosts = append(grpcHosts, host)
			grpcRules = append(grpcRules, ingressRule(box.Name, host, extra.Name))
			continue
		}
		httpHosts = append(httpHosts, host)
		httpRules = append(httpRules, ingressRule(box.Name, host, extra.Name))
	}

	if len(httpRules) > 0 {
		httpIngress = r.ingress(box, box.Name, httpHosts, httpRules, "HTTP")
	}
	if len(grpcRules) > 0 {
		grpcIngress = r.ingress(box, box.Name+IngressGRPCSuffix, grpcHosts, grpcRules, ProtocolGRPC)
	}
	return httpIngress, grpcIngress
}

// ingressRule forwards a whole host to one named Service port.
func ingressRule(service, host, portName string) networkingv1.IngressRule {
	prefix := networkingv1.PathTypePrefix
	return networkingv1.IngressRule{
		Host: host,
		IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     "/",
					PathType: &prefix,
					Backend: networkingv1.IngressBackend{
						Service: &networkingv1.IngressServiceBackend{
							Name: service,
							Port: networkingv1.ServiceBackendPort{Name: portName},
						},
					},
				}},
			},
		},
	}
}

// ingress assembles one Ingress. The TLS entry names the hosts but no secret:
// the controller falls back to its default certificate, which is how every
// wildcard route on a shared ingress already works.
func (r *DevBoxReconciler) ingress(
	box *devpilotv1alpha1.DevBox, name string, hosts []string, rules []networkingv1.IngressRule, protocol string,
) *networkingv1.Ingress {
	for i := range rules {
		rules[i].HTTP.Paths[0].Backend.Service.Name = box.Name
	}
	annotations := map[string]string{
		// Long running kernels, notebook downloads and websockets only work
		// without a proxy buffer and with a generous idle timeout.
		"nginx.ingress.kubernetes.io/proxy-body-size":    "0",
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "3600",
		"nginx.ingress.kubernetes.io/proxy-send-timeout": "3600",
		"nginx.ingress.kubernetes.io/proxy-buffering":    "off",
	}
	if protocol == ProtocolGRPC {
		annotations["nginx.ingress.kubernetes.io/backend-protocol"] = "GRPC"
	}
	class := r.Config.IngressClass
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   box.Namespace,
			Labels:      map[string]string{LabelManagedBy: ValueManagedBy, LabelDevBoxName: box.Name},
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			TLS:              []networkingv1.IngressTLS{{Hosts: hosts}},
			Rules:            rules,
		},
	}
}

// ensureIngress creates or updates one Ingress the operator owns.
func (r *DevBoxReconciler) ensureIngress(ctx context.Context, box *devpilotv1alpha1.DevBox, wanted *networkingv1.Ingress) error {
	log := logf.FromContext(ctx)
	existing := &networkingv1.Ingress{}
	err := r.Get(ctx, client.ObjectKeyFromObject(wanted), existing)
	if isNotFound(err) {
		if ownerErr := ctrlSetOwner(wanted, box, r.Scheme); ownerErr != nil {
			return fmt.Errorf("set owner reference on ingress %s: %w", wanted.Name, ownerErr)
		}
		if createErr := r.Create(ctx, wanted); createErr != nil && !isAlreadyExists(createErr) {
			return fmt.Errorf("create ingress %s: %w", wanted.Name, createErr)
		}
		log.Info("Created ingress", "ingress", wanted.Name, "hosts", ingressHosts(wanted))
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ingress %s: %w", wanted.Name, err)
	}
	if existing.Labels[LabelManagedBy] != ValueManagedBy {
		// Same rule as the Service: an Ingress the operator did not create stays
		// the user's.
		return nil
	}
	if equality.Semantic.DeepEqual(existing.Spec, wanted.Spec) &&
		equality.Semantic.DeepEqual(existing.Annotations, wanted.Annotations) {
		return nil
	}
	existing.Spec = wanted.Spec
	existing.Annotations = wanted.Annotations
	if updateErr := r.Update(ctx, existing); updateErr != nil {
		return fmt.Errorf("update ingress %s: %w", wanted.Name, updateErr)
	}
	log.Info("Updated ingress", "ingress", wanted.Name, "hosts", ingressHosts(wanted))
	return nil
}

// withdrawIngress removes an Ingress the operator created, and only that one.
func (r *DevBoxReconciler) withdrawIngress(ctx context.Context, box *devpilotv1alpha1.DevBox, name string) error {
	log := logf.FromContext(ctx)
	existing := &networkingv1.Ingress{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: name}, existing); {
	case err == nil:
		if existing.Labels[LabelManagedBy] != ValueManagedBy {
			return nil
		}
		if delErr := r.Delete(ctx, existing); delErr != nil && !isNotFound(delErr) {
			return fmt.Errorf("delete ingress %s: %w", name, delErr)
		}
		log.Info("Deleted ingress", "ingress", name)
	case isNotFound(err):
	default:
		return fmt.Errorf("get ingress %s: %w", name, err)
	}
	return nil
}

// httpHost is the public hostname a port is published on: the port name, the
// devbox and its namespace, under the configured domain. Two projects may both
// run "dev", and one project may expose "api" on two devboxes, so all three
// parts are needed to stay unique.
func (r *DevBoxReconciler) httpHost(box *devpilotv1alpha1.DevBox, prefix string) string {
	label := dnsLabel(prefix + "-" + box.Name + "-" + box.Namespace)
	return label + "." + r.Config.IngressDomain
}

// dnsLabel folds raw into a DNS-1123 label. A label is capped at 63 characters,
// so a long one keeps a readable prefix and a hash of the whole name: two
// devboxes sharing a prefix must not land on one host.
func dnsLabel(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "devbox"
	}
	if len(out) <= maxDNSLabelLen {
		return out
	}
	sum := sha256.Sum256([]byte(raw))
	suffix := "-" + hex.EncodeToString(sum[:hostHashLen/2])
	keep := maxDNSLabelLen - len(suffix)
	return strings.Trim(out[:keep], "-") + suffix
}

// httpURL renders a host into a URL, omitting the port when it is the scheme
// default.
func (r *DevBoxReconciler) httpURL(host, scheme string) string {
	port := r.Config.IngressPort
	if port == 0 || port == 443 {
		return scheme + "://" + host + "/"
	}
	return fmt.Sprintf("%s://%s:%d/", scheme, host, port)
}

// httpEndpoints returns the URLs the devbox is reachable on over HTTP, or empty
// values when the operator publishes none.
func (r *DevBoxReconciler) httpEndpoints(box *devpilotv1alpha1.DevBox) (string, map[string]string) {
	if !r.Config.HTTPExposed() {
		return "", nil
	}
	var jupyter string
	if jupyterEnabled(box) {
		jupyter = r.httpURL(r.httpHost(box, JupyterServicePortName), "https")
	}
	ports := map[string]string{}
	for _, extra := range httpPorts(box) {
		scheme := "https"
		if extra.Protocol == ProtocolGRPC {
			scheme = "grpcs"
		}
		ports[extra.Name] = r.httpURL(r.httpHost(box, extra.Name), scheme)
	}
	if len(ports) == 0 {
		ports = nil
	}
	return jupyter, ports
}
