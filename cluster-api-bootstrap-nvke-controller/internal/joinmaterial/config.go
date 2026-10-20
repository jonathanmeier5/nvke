// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package joinmaterial

import (
	"fmt"
	"strings"
)

// DefaultCRISocket is the socket the node images run containerd on. kubeadm
// refuses to guess when more than one runtime endpoint is present, so it is
// always stated.
const DefaultCRISocket = "unix:///run/containerd/containerd.sock"

// JoinConfigInput is what a worker's JoinConfiguration needs.
type JoinConfigInput struct {
	// APIServerEndpoint is host:port of the control plane, normally the VIP.
	APIServerEndpoint string

	// Token is the bootstrap token in id.secret form.
	Token string

	// CACertHashes are the discovery hashes, "sha256:<hex>".
	CACertHashes []string

	// NodeName is the name the kubelet registers under. It must match the
	// Machine, or Cluster API never matches the Node to it.
	NodeName string

	// NodeIP is advertised as the kubelet's address. A node with more than one
	// interface otherwise picks one by itself, and it is not always the one the
	// control plane can reach.
	NodeIP string

	// CRISocket defaults to DefaultCRISocket when empty.
	CRISocket string
}

// RenderJoinConfiguration returns the kubeadm JoinConfiguration document for a
// worker.
//
// v1beta3 is deliberate: it is what the NVKE controller emits today for the
// same node images, so a node joined by this provider and one joined by the
// controller are configured the same way.
func RenderJoinConfiguration(in JoinConfigInput) (string, error) {
	if in.APIServerEndpoint == "" {
		return "", fmt.Errorf("the API server endpoint is required")
	}
	if in.Token == "" {
		return "", fmt.Errorf("the bootstrap token is required")
	}
	if len(in.CACertHashes) == 0 {
		return "", fmt.Errorf("at least one CA certificate hash is required")
	}
	if in.NodeName == "" {
		return "", fmt.Errorf("the node name is required")
	}
	criSocket := in.CRISocket
	if criSocket == "" {
		criSocket = DefaultCRISocket
	}

	var b strings.Builder
	b.WriteString("apiVersion: kubeadm.k8s.io/v1beta3\n")
	b.WriteString("kind: JoinConfiguration\n")
	b.WriteString("discovery:\n")
	b.WriteString("  bootstrapToken:\n")
	fmt.Fprintf(&b, "    apiServerEndpoint: %s\n", in.APIServerEndpoint)
	fmt.Fprintf(&b, "    token: %s\n", in.Token)
	b.WriteString("    caCertHashes:\n")
	for _, hash := range in.CACertHashes {
		fmt.Fprintf(&b, "    - %s\n", hash)
	}
	b.WriteString("  timeout: 5m0s\n")
	// kubeadm uses this for the TLS bootstrap as well as discovery; leaving it
	// unset makes it fall back to a separate lookup that this token cannot
	// satisfy.
	fmt.Fprintf(&b, "  tlsBootstrapToken: %s\n", in.Token)
	b.WriteString("nodeRegistration:\n")
	fmt.Fprintf(&b, "  name: %s\n", in.NodeName)
	fmt.Fprintf(&b, "  criSocket: %s\n", criSocket)
	if in.NodeIP != "" {
		b.WriteString("  kubeletExtraArgs:\n")
		fmt.Fprintf(&b, "    node-ip: %s\n", in.NodeIP)
	}
	return b.String(), nil
}
