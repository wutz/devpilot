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

// Package sshhost serves SSH inside a DevBox: it owns the host key that makes a
// devbox keep its identity across restarts and the server that accepts the
// user's public keys.
package sshhost

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// KeyType is the OpenSSH type name Go emits for ed25519 keys.
const KeyType = "ssh-ed25519"

// SSHAuthorizedKeysFile is the conventional name for the file the keys are
// read from, matching what sshd itself expects.
const SSHAuthorizedKeysFile = "authorized_keys"

// GenerateHostKey returns a new ed25519 private key in the OpenSSH PEM format
// sshd and golang.org/x/crypto/ssh both read, plus its authorized_keys line.
//
// ed25519 rather than RSA: the key is generated in-process, and a 4096 bit RSA
// key costs hundreds of milliseconds where ed25519 is effectively free.
func GenerateHostKey(comment string) (private, public []byte, err error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(key, comment)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("derive public host key: %w", err)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		pub += " " + comment
	}
	return pem.EncodeToMemory(block), []byte(pub + "\n"), nil
}

// ParseHostKey loads a private host key and checks it can sign.
func ParseHostKey(private []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	return signer, nil
}

// AuthorizedKeys parses one public key per line, ignoring blanks and comments.
// A malformed key is an error: silently dropping it would leave the user locked
// out of a devbox that reports Running.
func AuthorizedKeys(lines []string) ([]ssh.PublicKey, error) {
	keys := make([]ssh.PublicKey, 0, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("parse authorized key %d: %w", i+1, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}
