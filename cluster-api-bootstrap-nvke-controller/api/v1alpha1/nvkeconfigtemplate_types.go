// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NvkeConfigTemplateResource is the template body a MachineDeployment clones.
type NvkeConfigTemplateResource struct {
	Spec NvkeConfigSpec `json:"spec,omitempty"`
}

// NvkeConfigTemplateSpec wraps the template body.
type NvkeConfigTemplateSpec struct {
	Template NvkeConfigTemplateResource `json:"template"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=nvkeconfigtemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion

// NvkeConfigTemplate is the per-pool bootstrap configuration. A
// MachineDeployment references it, and Cluster API clones one NvkeConfig per
// Machine from it.
//
// The attestation method lives here rather than on the infrastructure
// template: it decides how the agent proves its identity, which is bootstrap
// behaviour. It is also the Cluster API analogue of the node pool, which is
// where the NVKE controller carries the same setting today.
type NvkeConfigTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NvkeConfigTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// NvkeConfigTemplateList is a list of NvkeConfigTemplate.
type NvkeConfigTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NvkeConfigTemplate `json:"items"`
}
