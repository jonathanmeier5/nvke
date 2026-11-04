// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// adoptableRequest is the smallest request adoptCluster accepts: a VIP to reach
// the cluster through, and both secret refs.
func adoptableRequest() *api.CreateClusterRequest {
	req := validForgeCreateClusterRequest()
	req.ControlPlaneVIP = "10.0.0.10"
	cp := validForgeControlPlaneConfig()
	cp.KubeconfigSecretRef = &api.SecretReference{Name: "csc-kubeconfig"}
	cp.EtcdSecretRef = &api.SecretReference{Name: "csc-etcd"}
	req.ControlPlaneConfig = cp
	return req
}

func TestValidateAdoptionFields(t *testing.T) {
	t.Run("accepts a complete adopt request", func(t *testing.T) {
		require.NoError(t, validateAdoptionFields(adoptableRequest(), ClusterIntentAdopt))
	})

	t.Run("adopt requires controlPlaneConfig", func(t *testing.T) {
		req := adoptableRequest()
		req.ControlPlaneConfig = nil
		require.ErrorIs(t, validateAdoptionFields(req, ClusterIntentAdopt), errAdoptNeedsControlPlaneConfig)
	})

	t.Run("adopt requires a kubeconfig ref", func(t *testing.T) {
		req := adoptableRequest()
		req.ControlPlaneConfig.KubeconfigSecretRef = nil
		require.ErrorIs(t, validateAdoptionFields(req, ClusterIntentAdopt), errAdoptNeedsKubeconfigRef)
	})

	t.Run("adopt requires an etcd ref", func(t *testing.T) {
		req := adoptableRequest()
		req.ControlPlaneConfig.EtcdSecretRef = nil
		require.ErrorIs(t, validateAdoptionFields(req, ClusterIntentAdopt), errAdoptNeedsEtcdRef)
	})

	t.Run("adopt requires a control-plane VIP", func(t *testing.T) {
		req := adoptableRequest()
		req.ControlPlaneVIP = ""
		require.ErrorIs(t, validateAdoptionFields(req, ClusterIntentAdopt), errAdoptNeedsVIP)
	})

	t.Run("create rejects the adopt-only secret refs", func(t *testing.T) {
		req := adoptableRequest()
		require.ErrorIs(t, validateAdoptionFields(req, ClusterIntentCreate), errSecretRefsAdoptOnly)
	})

	t.Run("create without the refs is untouched", func(t *testing.T) {
		req := validForgeCreateClusterRequest()
		req.ControlPlaneConfig = validForgeControlPlaneConfig()
		require.NoError(t, validateAdoptionFields(req, ClusterIntentCreate))
	})
}

func TestMarkClusterAdopted(t *testing.T) {
	cluster, err := DBClusterFromAPI(adoptableRequest())
	require.NoError(t, err)

	// A created cluster starts out provisioning.
	require.Equal(t, db.ConditionStatusTrue, cluster.Conditions.Provisioning.Status)
	require.Equal(t, db.ConditionStatusFalse, cluster.Conditions.Provisioned.Status)

	markClusterAdopted(&cluster)

	// Adoption keeps the ordinary lifecycle: ProvisionCluster still has to apply
	// the CAPI scaffold, and it is the workflow that marks the cluster
	// Provisioned. Only the message distinguishes an adopted cluster here.
	require.Equal(t, db.ConditionStatusTrue, cluster.Conditions.Provisioning.Status)
	require.Equal(t, db.ConditionStatusFalse, cluster.Conditions.Provisioned.Status)
	require.Contains(t, cluster.Conditions.Provisioning.Message, "Adopting")
}

// Adoption must not demand a recipe for building control-plane nodes it will
// never build; create still must.
func TestMachineTemplateOptionalOnlyWhenAdopting(t *testing.T) {
	cfg := func() *api.CreateClusterControlPlaneConfig {
		c := validForgeControlPlaneConfig()
		c.Nico = nil
		c.Infra = &api.InfraControlPlaneProvider{}
		return c
	}

	err := validateCreateCAPIControlPlaneConfig(db.ProviderTypeCAPI, cfg(), ClusterIntentCreate)
	require.EqualError(t, err, "controlPlaneConfig.infra.machineTemplate is required")

	require.NoError(t, validateCreateCAPIControlPlaneConfig(db.ProviderTypeCAPI, cfg(), ClusterIntentAdopt))
}

// The nico mirror of the machineTemplate case: instanceTypePreference,
// osProfile and the subnet/VPC-prefix choice all describe how to build a
// control-plane node, so adoption must not demand them. Provider exclusivity
// still applies.
func TestNicoControlPlaneFieldsOptionalOnlyWhenAdopting(t *testing.T) {
	empty := func() *api.CreateClusterControlPlaneConfig {
		return &api.CreateClusterControlPlaneConfig{DesiredCount: 3, Nico: &api.NicoControlPlaneProviderCreate{}}
	}

	err := validateCreateForgeControlPlaneConfigFields(empty(), ClusterIntentCreate)
	require.Error(t, err)
	require.Contains(t, err.Error(), "instanceTypePreference")

	require.NoError(t, validateCreateForgeControlPlaneConfigFields(empty(), ClusterIntentAdopt))

	// An adopt request still cannot mix providers.
	mixed := empty()
	mixed.Infra = &api.InfraControlPlaneProvider{}
	require.Error(t, validateCreateForgeControlPlaneConfigFields(mixed, ClusterIntentAdopt))
}

func TestApplyAdoptionToSpec(t *testing.T) {
	req := adoptableRequest()
	spec := db.ControlPlaneConfigSpec{}

	applyAdoptionToSpec(&spec, req.ControlPlaneConfig)

	require.True(t, spec.Adopted)
	require.NotNil(t, spec.KubeconfigSecretRef)
	require.Equal(t, "csc-kubeconfig", spec.KubeconfigSecretRef.Name)
	require.NotNil(t, spec.EtcdSecretRef)
	require.Equal(t, "csc-etcd", spec.EtcdSecretRef.Name)

	// The refs are part of the snapshot, but must not look like a node-replacing
	// change: adopting must never schedule a control-plane rollout.
	require.False(t, db.IsControlPlaneRolloutRequired(db.ControlPlaneConfigSpec{}, spec))
}
