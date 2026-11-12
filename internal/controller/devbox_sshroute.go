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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sshgatewayv1alpha1 "github.com/wutz/sshgateway/api/v1alpha1"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// SSHServicePortName is the port name of the Service that fronts a devbox.
	SSHServicePortName = "ssh"
	// DefaultSSHGatewayNamespace is where the gateway operator serves
	// SSHGateways when spec.access.ssh.gatewayRef.namespace is empty.
	DefaultSSHGatewayNamespace = "sshgateway-system"
	// GatewayIdentitySecretName holds the key the gateway logs in to backends
	// with. It lives in the gateway namespace and is created by the gateway
	// operator on first use.
	GatewayIdentitySecretName = "sshgateway-identity"
	// GatewayIdentityPublicKeyKey is the ServiceAccount-style key inside it. The
	// devbox lists this key in authorized_keys so the gateway may log in.
	GatewayIdentityPublicKeyKey = "ssh-publickey"

	// ConditionSSHRouteReady tracks publishing the devbox on the gateway.
	ConditionSSHRouteReady = "SSHRouteReady"

	// maxSSHUsernameLen is the ceiling the SSHRoute schema puts on usernames.
	maxSSHUsernameLen = 32
)

// sshGatewayRef returns the gateway to publish on, or nil when the devbox is
// not published. A gatewayRef on a devbox that does not serve SSH is ignored:
// there is nothing for the gateway to connect to.
func sshGatewayRef(box *devpilotv1alpha1.DevBox) *devpilotv1alpha1.NamespacedName {
	if !sshEnabled(box) || box.Spec.Access.SSH.GatewayRef == nil {
		return nil
	}
	ref := box.Spec.Access.SSH.GatewayRef
	if ref.Name == "" {
		return nil
	}
	return ref
}

// gatewayNamespace is the namespace the referenced gateway lives in.
func gatewayNamespace(ref *devpilotv1alpha1.NamespacedName) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return DefaultSSHGatewayNamespace
}

// ensureSSHAccess publishes the devbox on its gateway, or withdraws it when the
// gatewayRef is gone. The Service the gateway resolves the backend through is
// the shared access Service: it is named after the DevBox because the operator
// finds the host key of a backend at <service>-hostkey, and <devbox>-hostkey is
// the Secret the devbox already keeps.
func (r *DevBoxReconciler) ensureSSHAccess(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	ref := sshGatewayRef(box)
	if ref == nil {
		return r.withdrawSSHAccess(ctx, box)
	}
	route, err := r.ensureSSHRoute(ctx, box, ref)
	if err != nil {
		return err
	}
	r.observeSSHRoute(box, route)
	return nil
}

// withdrawSSHAccess removes the route a previous gatewayRef left behind. The
// object carries an owner reference, so only a DevBox that outlives the removal
// of its gatewayRef needs this. The Service stays: it also fronts the devbox's
// HTTP endpoints, and ensureAccessService decides when it goes.
func (r *DevBoxReconciler) withdrawSSHAccess(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	log := logf.FromContext(ctx)
	key := client.ObjectKey{Namespace: box.Namespace, Name: box.Name}

	route := &sshgatewayv1alpha1.SSHRoute{}
	switch err := r.Get(ctx, key, route); {
	case err == nil:
		if delErr := r.Delete(ctx, route); delErr != nil && !isNotFound(delErr) {
			return fmt.Errorf("delete SSHRoute %s: %w", route.Name, delErr)
		}
		log.Info("Deleted SSHRoute", "route", route.Name)
	case isNotFound(err), meta.IsNoMatchError(err):
		// Nothing published, or the gateway API is not installed: nothing to do.
	default:
		return fmt.Errorf("get SSHRoute %s: %w", key.Name, err)
	}
	return nil
}

// ensureSSHRoute creates or updates the route that forwards a gateway username
// to the devbox. A nil route means nothing was published.
func (r *DevBoxReconciler) ensureSSHRoute(
	ctx context.Context, box *devpilotv1alpha1.DevBox, ref *devpilotv1alpha1.NamespacedName,
) (*sshgatewayv1alpha1.SSHRoute, error) {
	log := logf.FromContext(ctx)
	if len(box.Spec.Access.SSH.AuthorizedKeys) == 0 {
		// A route without keys would be refused by the schema and, worse, would
		// look published while nobody could log in.
		return nil, nil
	}

	wanted := sshRoute(box, ref)
	existing := &sshgatewayv1alpha1.SSHRoute{}
	err := r.Get(ctx, client.ObjectKeyFromObject(wanted), existing)
	if isNotFound(err) {
		if ownerErr := ctrlSetOwner(wanted, box, r.Scheme); ownerErr != nil {
			return nil, fmt.Errorf("set owner reference on SSHRoute %s: %w", wanted.Name, ownerErr)
		}
		if createErr := r.Create(ctx, wanted); createErr != nil {
			if isAlreadyExists(createErr) {
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(wanted), existing); getErr != nil {
					return nil, getErr
				}
				return existing, nil
			}
			if meta.IsNoMatchError(createErr) || isNotFound(createErr) {
				// The gateway API is not installed. SSH stays reachable on the
				// pod, so this is a degraded state, not a failure.
				log.Info("SSH gateway API is not installed; skipping the SSHRoute", "gateway", ref.Name)
				return nil, nil
			}
			return nil, fmt.Errorf("create SSHRoute %s: %w", wanted.Name, createErr)
		}
		log.Info("Created SSHRoute", "route", wanted.Name, "gateway", ref.Name)
		return wanted, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get SSHRoute %s: %w", wanted.Name, err)
	}
	if equality.Semantic.DeepEqual(existing.Spec, wanted.Spec) {
		return existing, nil
	}
	existing.Spec = wanted.Spec
	if updateErr := r.Update(ctx, existing); updateErr != nil {
		return nil, fmt.Errorf("update SSHRoute %s: %w", wanted.Name, updateErr)
	}
	log.Info("Updated SSHRoute", "route", wanted.Name, "gateway", ref.Name)
	return existing, nil
}

// sshRoute is the route that publishes box on the gateway ref names.
func sshRoute(box *devpilotv1alpha1.DevBox, ref *devpilotv1alpha1.NamespacedName) *sshgatewayv1alpha1.SSHRoute {
	namespace := gatewayNamespace(ref)
	return &sshgatewayv1alpha1.SSHRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      box.Name,
			Namespace: box.Namespace,
			Labels:    map[string]string{LabelManagedBy: ValueManagedBy, LabelDevBoxName: box.Name},
		},
		Spec: sshgatewayv1alpha1.SSHRouteSpec{
			ParentRefs: []sshgatewayv1alpha1.ParentReference{{
				Name:      ref.Name,
				Namespace: &namespace,
			}},
			Rules: []sshgatewayv1alpha1.SSHRouteRule{{
				Matches:        []sshgatewayv1alpha1.SSHRouteMatch{{Username: sshRouteUsername(box)}},
				AuthorizedKeys: append([]string{}, box.Spec.Access.SSH.AuthorizedKeys...),
				BackendRefs: []sshgatewayv1alpha1.SSHBackendRef{{
					Name:     box.Name,
					Port:     SSHPort,
					Username: sshBackendUsername(box),
				}},
			}},
		},
	}
}

// sshRouteUsername is the name a user logs in with at the gateway. It carries
// the namespace because a username is claimed by the first route that uses it
// across the whole gateway, and two projects may both run a devbox called
// "dev". The readable form is kept whenever it fits; longer names keep a
// prefix plus a hash of the whole one, so two devboxes sharing a prefix do not
// collide.
func sshRouteUsername(box *devpilotv1alpha1.DevBox) string {
	base := sanitizeUsername(box.Namespace + "-" + box.Name)
	if len(base) <= maxSSHUsernameLen {
		return base
	}
	sum := sha256.Sum256([]byte(box.Namespace + "/" + box.Name))
	return base[:maxSSHUsernameLen-len("-")-2*4] + "-" + hex.EncodeToString(sum[:4])
}

// sanitizeUsername folds a string into the SSHRoute username pattern
// ^[a-z_][a-z0-9_-]{0,31}$. It is deterministic, so a reconcile that changes
// nothing keeps the name a user already put in their SSH config.
func sanitizeUsername(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "devbox"
	}
	if first := out[0]; (first < 'a' || first > 'z') && first != '_' {
		out = "devbox-" + out
	}
	return out
}

// sshBackendUsername is the account the gateway logs into the devbox as. The
// route schema accepts lowercase names only, and a name outside that form would
// be refused at admission, so it falls back to root, which is what the built-in
// server runs as regardless.
func sshBackendUsername(box *devpilotv1alpha1.DevBox) string {
	name := sanitizeUsername(sshUsername(box))
	if name != sshUsername(box) {
		return "root"
	}
	return name
}

// observeSSHRoute records whether the gateway accepted the route and resolved
// its backend. A nil route means the devbox is not published.
func (r *DevBoxReconciler) observeSSHRoute(box *devpilotv1alpha1.DevBox, route *sshgatewayv1alpha1.SSHRoute) {
	if sshGatewayRef(box) == nil {
		return
	}
	if route == nil {
		if len(box.Spec.Access.SSH.AuthorizedKeys) == 0 {
			r.setCondition(box, ConditionSSHRouteReady, metav1ConditionFalse, "NoAuthorizedKeys",
				"spec.access.ssh.authorizedKeys is empty, so the gateway route was not created")
			return
		}
		r.setCondition(box, ConditionSSHRouteReady, metav1ConditionFalse, "GatewayAPIAbsent",
			"the sshgateway.wutz.dev API is not installed; SSH is reachable on the pod only")
		return
	}

	if len(route.Status.Parents) == 0 {
		r.setCondition(box, ConditionSSHRouteReady, metav1ConditionFalse, "RoutePending",
			"the gateway has not reported on the route yet")
		return
	}
	// One parent is configured, so only its report matters; a gateway that
	// attaches the route somewhere else is not what the devbox asked for.
	status, reason, message := sshRouteCondition(route.Status.Parents[0].Conditions, sshRouteUsername(box))
	r.setCondition(box, ConditionSSHRouteReady, status, reason, message)
}

// sshRouteCondition maps the gateway's conditions on a route to the devbox
// condition. The reason and message come from the gateway when it reported one,
// so the user sees the gateway's own words in a failure.
func sshRouteCondition(conditions []metav1.Condition, username string) (metav1.ConditionStatus, string, string) {
	accepted := meta.FindStatusCondition(conditions, sshgatewayv1alpha1.SSHConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		if accepted != nil {
			return metav1ConditionFalse, accepted.Reason, accepted.Message
		}
		return metav1ConditionFalse, "RouteNotAccepted", "the gateway has not accepted the route yet"
	}
	resolved := meta.FindStatusCondition(conditions, sshgatewayv1alpha1.SSHConditionResolvedRefs)
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		if resolved != nil {
			return metav1ConditionFalse, resolved.Reason, resolved.Message
		}
		return metav1ConditionFalse, "BackendNotResolved", "the gateway cannot resolve the devbox backend yet"
	}
	return metav1ConditionTrue, "RouteAccepted", "The gateway forwards " + username + " to this devbox"
}

// sshGatewayEndpoint returns the user@host:port a client reaches box on through
// its gateway, or "" while the route or the gateway address is not ready.
func (r *DevBoxReconciler) sshGatewayEndpoint(ctx context.Context, box *devpilotv1alpha1.DevBox) string {
	ref := sshGatewayRef(box)
	if ref == nil {
		return ""
	}
	route := &sshgatewayv1alpha1.SSHRoute{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: box.Name}, route); err != nil {
		return ""
	}
	if len(route.Status.Parents) == 0 {
		return ""
	}
	resolved := meta.FindStatusCondition(route.Status.Parents[0].Conditions, sshgatewayv1alpha1.SSHConditionResolvedRefs)
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		return ""
	}
	gateway := &sshgatewayv1alpha1.SSHGateway{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gatewayNamespace(ref), Name: ref.Name}, gateway); err != nil {
		return ""
	}
	if len(gateway.Status.Addresses) == 0 {
		return ""
	}
	return fmt.Sprintf("%s@%s", sshRouteUsername(box), gateway.Status.Addresses[0])
}

// gatewayIdentityKey returns the public key the gateway logs into the devbox
// with, or "" when the devbox is not published or the gateway has not created
// its identity yet.
func (r *DevBoxReconciler) gatewayIdentityKey(ctx context.Context, box *devpilotv1alpha1.DevBox) (string, error) {
	ref := sshGatewayRef(box)
	if ref == nil {
		return "", nil
	}
	identity := &corev1.Secret{}
	key := client.ObjectKey{Namespace: gatewayNamespace(ref), Name: GatewayIdentitySecretName}
	if err := r.Get(ctx, key, identity); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get gateway identity %s/%s: %w", key.Namespace, key.Name, err)
	}
	return strings.TrimSpace(string(identity.Data[GatewayIdentityPublicKeyKey])), nil
}
