// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AttestationMethod selects how the node's SPIRE agent proves its identity.
// The values match the methods the NVKE controller already models on a node
// pool (internal/db/node_pools.go), so a node bootstrapped here attests the
// same way as one the controller provisions.
// +kubebuilder:validation:Enum=tpm;joinToken;dsxImds
type AttestationMethod string

const (
	AttestationMethodTPM       AttestationMethod = "tpm"
	AttestationMethodJoinToken AttestationMethod = "joinToken"
	AttestationMethodDSXIMDS   AttestationMethod = "dsxImds"
)

// NvkeConfigSpec is the bootstrap configuration for one Machine.
//
// The data secret this produces carries only what the host needs to boot,
// start nke-agent and attest. The kubeadm join configuration is not in it: the
// join is driven over gRPC once the agent answers, so the join material is
// never written to a secret at rest, and it is built after the control-plane
// endpoint is known rather than before.
type NvkeConfigSpec struct {
	// AttestationMethod selects the SPIRE node attestor. Defaults to tpm.
	// +optional
	AttestationMethod AttestationMethod `json:"attestationMethod,omitempty"`

	// KubeletExtraArgs are appended to the kubelet's flags at join time.
	// +optional
	KubeletExtraArgs map[string]string `json:"kubeletExtraArgs,omitempty"`

	// NodeLabels are applied to the Kubernetes Node after it joins.
	// +optional
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`

	// NodeTaints are applied to the Kubernetes Node after it joins.
	// +optional
	NodeTaints []corev1Taint `json:"nodeTaints,omitempty"`
}

// corev1Taint mirrors corev1.Taint. It is declared here so the API package does
// not pull k8s.io/api into every consumer; replace with corev1.Taint if that
// dependency becomes unavoidable.
type corev1Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// NvkeConfigStatus reports the bootstrap contract's two signals plus the
// progress of the join this provider drives itself.
type NvkeConfigStatus struct {
	// Initialization reports dataSecretCreated, which is what the Machine
	// controller waits on before infrastructure is provisioned.
	// +optional
	Initialization NvkeConfigInitializationStatus `json:"initialization,omitempty,omitzero"`

	// DataSecretName is the secret holding the boot configuration.
	// +optional
	DataSecretName string `json:"dataSecretName,omitempty"`

	// Conditions follows the Cluster API condition conventions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NvkeConfigInitializationStatus is the contract's initialization block.
type NvkeConfigInitializationStatus struct {
	// DataSecretCreated is true once the secret exists and is named above.
	// +optional
	DataSecretCreated *bool `json:"dataSecretCreated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=nvkeconfigs,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion

// NvkeConfig is the bootstrap configuration for one Machine.
type NvkeConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NvkeConfigSpec   `json:"spec,omitempty"`
	Status NvkeConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NvkeConfigList is a list of NvkeConfig.
type NvkeConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NvkeConfig `json:"items"`
}

// GetConditions returns the conditions, for the Cluster API condition helpers.
func (c *NvkeConfig) GetConditions() []metav1.Condition { return c.Status.Conditions }

// SetConditions sets the conditions, for the Cluster API condition helpers.
func (c *NvkeConfig) SetConditions(conditions []metav1.Condition) { c.Status.Conditions = conditions }
