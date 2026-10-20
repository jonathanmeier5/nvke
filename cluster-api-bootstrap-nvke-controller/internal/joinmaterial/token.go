// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package joinmaterial produces what a worker needs to join: a bootstrap token
// minted against the workload cluster, the cluster CA's public-key hash, and
// the kubeadm JoinConfiguration that carries both.
//
// The token comes from the workload cluster's own API, the way the kubeadm
// bootstrap provider does it, rather than from a control-plane node's agent.
// That keeps the join independent of any particular node being reachable.
package joinmaterial

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tokenTTL bounds how long a minted token stays usable. It only has to cover
// one join, and a token that outlives its node is a credential left lying
// around, so this is deliberately short.
const tokenTTL = 15 * time.Minute

// bootstrapTokenGroup is the group kubeadm expects a node-bootstrap token to
// carry. Without it the kubelet's CSR is never auto-approved.
const bootstrapTokenGroup = "system:bootstrappers:kubeadm:default-node-token"

// tokenIDChars is kubeadm's alphabet for both halves of a token. The format is
// fixed: six characters, a dot, sixteen characters.
const tokenIDChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// Token is a kubeadm bootstrap token, split the way the Secret stores it.
type Token struct {
	ID     string
	Secret string
}

// String renders the token in the "id.secret" form kubeadm takes on the
// command line and in a JoinConfiguration.
func (t Token) String() string { return t.ID + "." + t.Secret }

// MintToken creates a bootstrap token in the workload cluster and returns it.
//
// The token is written as a Secret in kube-system, which is where the API
// server's bootstrap authenticator reads them from. It expires on its own, so
// nothing has to clean it up: a failed join leaves a token that stops working
// rather than one that lingers.
func MintToken(ctx context.Context, c client.Client) (Token, error) {
	id, err := randomString(6)
	if err != nil {
		return Token{}, fmt.Errorf("generating the token id: %w", err)
	}
	secret, err := randomString(16)
	if err != nil {
		return Token{}, fmt.Errorf("generating the token secret: %w", err)
	}
	token := Token{ID: id, Secret: secret}

	expiry := time.Now().Add(tokenTTL).UTC().Format(time.RFC3339)
	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-" + token.ID,
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id":                       token.ID,
			"token-secret":                   token.Secret,
			"expiration":                     expiry,
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              bootstrapTokenGroup,
			"description":                    "Minted by the NVKE bootstrap provider for one node join.",
		},
	}
	if err := c.Create(ctx, obj); err != nil {
		return Token{}, fmt.Errorf("creating the bootstrap token secret: %w", err)
	}
	return token, nil
}

// CACertHash returns the "sha256:<hex>" discovery hash for a PEM-encoded CA
// certificate.
//
// kubeadm hashes the certificate's SubjectPublicKeyInfo, not the certificate
// itself, so the value survives the CA being re-signed with the same key.
func CACertHash(caPEM []byte) (string, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return "", fmt.Errorf("the CA is not PEM encoded")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing the CA certificate: %w", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// randomString returns n characters from kubeadm's token alphabet.
//
// The modulo is unbiased here only because the alphabet is 36 characters and
// the source is a full byte, so the tail of the byte range is rejected rather
// than folded in.
func randomString(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	limit := byte(256 - (256 % len(tokenIDChars)))
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		if buf[0] >= limit {
			continue
		}
		out = append(out, tokenIDChars[int(buf[0])%len(tokenIDChars)])
	}
	return string(out), nil
}
