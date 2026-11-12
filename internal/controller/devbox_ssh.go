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
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
	"github.com/wutz/devpilot/internal/sshhost"
)

const (
	// SSHSecretSuffix is appended to the DevBox name for the host key secret.
	SSHSecretSuffix = "-hostkey"
	// SSHVolumeName is the pod volume backed by that secret.
	SSHVolumeName = "ssh-hostkey"
	// SSHMountPath must match initroot.SSHDir. It deliberately does not live
	// under InitMountPath: devpilot-init keeps that whole prefix out of the
	// merged root, so a key mounted there would vanish after pivot_root.
	SSHMountPath = "/run/devpilot-ssh"
	// SSHPort is the port the built-in server listens on.
	SSHPort int32 = 22
	// SSHPasswordMessage explains why passwordSecretRef does not take effect.
	SSHPasswordMessage = "passwordSecretRef requires an image that runs its own sshd; " +
		"the built-in server accepts keys only"
)

// Secret data keys. The names are what sshd itself looks for, so an image that
// does ship an sshd can point at the same volume without any translation.
const (
	SSHHostKeyKey      = "ssh_host_ed25519_key"
	SSHHostKeyPubKey   = "ssh_host_ed25519_key.pub"
	SSHAuthorizedKeyID = "authorized_keys"
)

// sshEnabled reports whether the pod should serve SSH.
func sshEnabled(box *devpilotv1alpha1.DevBox) bool {
	return box.Spec.Access.SSH != nil && box.Spec.Access.SSH.Enabled
}

// sshUsername is the login name sessions report. It is cosmetic for the
// built-in server, which runs everything as the pod's own uid.
func sshUsername(box *devpilotv1alpha1.DevBox) string {
	if box.Spec.Access.SSH != nil && box.Spec.Access.SSH.Username != "" {
		return box.Spec.Access.SSH.Username
	}
	return "root"
}

// ensureSSHSecret creates the host key on first use and keeps authorized_keys
// in step with the spec and with the key the gateway logs in with.
//
// The host key is generated once and never rotated: clients pin it in
// known_hosts, and a new key would greet every user with a host key changed
// warning that they cannot verify.
func (r *DevBoxReconciler) ensureSSHSecret(ctx context.Context, box *devpilotv1alpha1.DevBox) (*corev1.Secret, error) {
	log := logf.FromContext(ctx)
	name := box.Name + SSHSecretSuffix

	// The gateway key is read here rather than at start: the gateway operator
	// may create its identity after the devbox did, and the extra key must reach
	// a running pod without a restart, which reads authorized_keys per login.
	gatewayKey, err := r.gatewayIdentityKey(ctx, box)
	if err != nil {
		return nil, err
	}
	wanted := authorizedKeysData(box, gatewayKey)

	secret := &corev1.Secret{}
	err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: name}, secret)
	if isNotFound(err) {
		private, public, genErr := sshhost.GenerateHostKey("devpilot:" + box.Name)
		if genErr != nil {
			return nil, fmt.Errorf("generate SSH host key for %s: %w", box.Name, genErr)
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: box.Namespace,
				Labels:    map[string]string{LabelManagedBy: ValueManagedBy, LabelDevBoxName: box.Name},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				SSHHostKeyKey:      private,
				SSHHostKeyPubKey:   public,
				SSHAuthorizedKeyID: wanted,
			},
		}
		if ownerErr := ctrlSetOwner(secret, box, r.Scheme); ownerErr != nil {
			return nil, fmt.Errorf("set owner reference on secret %s: %w", name, ownerErr)
		}
		log.Info("Created SSH host key", "secret", name)
		if createErr := r.Create(ctx, secret); createErr != nil {
			if isAlreadyExists(createErr) {
				// A concurrent reconcile won the race; re-read so both agree.
				return r.ensureSSHSecret(ctx, box)
			}
			return nil, fmt.Errorf("create secret %s: %w", name, createErr)
		}
		return secret, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get secret %s: %w", name, err)
	}

	if _, ok := secret.Data[SSHHostKeyKey]; !ok {
		// Overwriting would invalidate every known_hosts entry the user has
		// accumulated. Deleting the secret is the deliberate way to rotate.
		return nil, fmt.Errorf("secret %s has no %s; delete it to regenerate the host key",
			name, SSHHostKeyKey)
	}

	if bytes.Equal(secret.Data[SSHAuthorizedKeyID], wanted) {
		return secret, nil
	}
	secret.Data[SSHAuthorizedKeyID] = wanted
	if updateErr := r.Update(ctx, secret); updateErr != nil {
		return nil, fmt.Errorf("update authorized keys in secret %s: %w", name, updateErr)
	}
	log.Info("Updated SSH authorized keys", "secret", name)
	return secret, nil
}

// authorizedKeysData renders the spec keys plus the gateway identity key as an
// authorized_keys file: one key per line, newline terminated. A CRD list cannot
// carry a trailing blank line, and sshd rejects a final line without one.
//
// The gateway key is appended last and deduplicated, so a spec key that already
// matches it does not appear twice.
func authorizedKeysData(box *devpilotv1alpha1.DevBox, extra ...string) []byte {
	var lines []string
	if box.Spec.Access.SSH != nil {
		lines = append(lines, box.Spec.Access.SSH.AuthorizedKeys...)
	}
	for _, line := range extra {
		line = strings.TrimSpace(line)
		if line != "" && !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// sshVolume exposes the host key secret to the main container, read-only.
func sshVolume(box *devpilotv1alpha1.DevBox) corev1.Volume {
	return corev1.Volume{
		Name: SSHVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  box.Name + SSHSecretSuffix,
				DefaultMode: new(int32(0o400)),
			},
		},
	}
}

// sshCommandFlags tells devpilot-init to serve SSH with the mounted key. An
// empty authorized_keys file is still passed: the server fails loudly instead
// of silently starting an endpoint nobody can log in to.
func sshCommandFlags() []string {
	return []string{
		fmt.Sprintf("--ssh-listen=:%d", SSHPort),
		"--ssh-host-key=" + SSHMountPath + "/" + SSHHostKeyKey,
		"--ssh-authorized-keys=" + SSHMountPath + "/" + SSHAuthorizedKeyID,
	}
}

// sshEndpoint is the address a client can reach while the pod runs. The
// gateway's stable route is added separately once an SSHGateway exists.
func sshEndpoint(box *devpilotv1alpha1.DevBox) string {
	return fmt.Sprintf("%s@%s%s.%s.pod:%d",
		sshUsername(box), box.Name, PodSuffix, box.Namespace, SSHPort)
}
