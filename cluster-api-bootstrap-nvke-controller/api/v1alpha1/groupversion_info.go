// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 holds the bootstrap API this provider serves.
//
// The provider satisfies the Cluster API bootstrap contract: it publishes a
// data secret and reports it on the NvkeConfig, which lets the Machine reach
// bootstrapReady and the infrastructure provider create the host.
// +kubebuilder:object:generate=true
// +groupName=bootstrap.cluster.x-k8s.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the bootstrap contract's API group. Cluster API finds a
	// bootstrap provider by group and kind, so this value is part of the
	// contract rather than a free choice.
	GroupName = "bootstrap.cluster.x-k8s.io"
)

var (
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"} //nolint:gochecknoglobals // Kubernetes API registration requires package variables.

	schemeBuilder = runtime.NewSchemeBuilder(addKnownTypes) //nolint:gochecknoglobals // Kubernetes API registration requires package variables.

	AddToScheme = schemeBuilder.AddToScheme //nolint:gochecknoglobals // Kubernetes API registration requires package variables.
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&NvkeConfig{}, &NvkeConfigList{},
		&NvkeConfigTemplate{}, &NvkeConfigTemplateList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
