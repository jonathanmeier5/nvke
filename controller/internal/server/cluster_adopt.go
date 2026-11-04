// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

var (
	errAdoptNeedsControlPlaneConfig = errors.New("controlPlaneConfig is required to adopt a cluster")
	errAdoptNeedsKubeconfigRef      = errors.New("controlPlaneConfig.kubeconfigSecretRef is required to adopt a cluster")
	errAdoptNeedsEtcdRef            = errors.New("controlPlaneConfig.etcdSecretRef is required to adopt a cluster")
	errAdoptNeedsVIP                = errors.New("controlPlaneVIP is required to adopt a cluster: the control-plane nodes are discovered through it")
	errSecretRefsAdoptOnly          = errors.New("controlPlaneConfig.kubeconfigSecretRef and controlPlaneConfig.etcdSecretRef are only accepted when adopting a cluster")
)

// AdoptCluster records a cluster that already runs. It shares its whole body with
// CreateCluster: the only differences are the extra required fields checked here,
// the adoption conditions the row is written with, and the control-plane build
// steps ProvisionCluster skips when it is told the cluster was adopted.
func (s *clusterService) AdoptCluster(ctx context.Context, req api.AdoptClusterRequestObject) (api.AdoptClusterResponseObject, error) {
	created, badRequest, err := s.createOrAdoptCluster(ctx, req.Body, ClusterIntentAdopt)
	if err != nil {
		return nil, err
	}
	if badRequest != "" {
		return api.AdoptCluster400JSONResponse{Message: badRequest}, nil
	}
	return api.AdoptCluster201JSONResponse(APIClusterFromDB(&created)), nil
}

// validateAdoptionFields enforces the fields that separate the two operations.
// The secret refs are adopt-only on the way in; the update schema has no field
// for either, so nothing can change them afterwards.
func validateAdoptionFields(req *api.CreateClusterRequest, intent ClusterIntent) error {
	cfg := req.ControlPlaneConfig

	if !intent.isAdopt() {
		if cfg != nil && (cfg.KubeconfigSecretRef != nil || cfg.EtcdSecretRef != nil) {
			return errSecretRefsAdoptOnly
		}
		return nil
	}

	// This guarded a deployment flag that no longer exists: the cluster-level
	// controlPlaneConfig is always persisted now, so the refs can no longer be
	// accepted and silently dropped.
	if cfg == nil {
		return errAdoptNeedsControlPlaneConfig
	}
	if cfg.KubeconfigSecretRef == nil {
		return errAdoptNeedsKubeconfigRef
	}
	if cfg.EtcdSecretRef == nil {
		return errAdoptNeedsEtcdRef
	}
	if req.ControlPlaneVIP == "" {
		return errAdoptNeedsVIP
	}
	return nil
}

// markClusterAdopted records why this cluster is provisioning. Its Kubernetes
// control plane is already serving, but the management plane still has work to
// do before the cluster is usable here: ProvisionCluster has to apply the CAPI
// scaffold that every Machine is reconciled against. So an adopted cluster goes
// through the ordinary lifecycle and is marked Provisioned by the workflow, not
// here. Claiming Provisioned up front would let worker pools start against a
// cluster with no CAPI parent object.
func markClusterAdopted(cluster *db.Cluster) {
	cluster.Conditions.Provisioning = db.NewCondition(true, "Adopting: the control plane already runs; building its CAPI representation")
	cluster.Conditions.Provisioned = db.NewCondition(false, "Not yet adopted")
}

// applyAdoptionToSpec records the adoption marker and the two secret refs on the
// control-plane config snapshot. The snapshot is jsonb, so these need no schema
// migration.
func applyAdoptionToSpec(spec *db.ControlPlaneConfigSpec, cfg *api.CreateClusterControlPlaneConfig) {
	spec.Adopted = true
	spec.KubeconfigSecretRef = dbSecretRefFromAPI(cfg.KubeconfigSecretRef)
	spec.EtcdSecretRef = dbSecretRefFromAPI(cfg.EtcdSecretRef)
}

func dbSecretRefFromAPI(ref *api.SecretReference) *db.SecretRef {
	if ref == nil {
		return nil
	}
	return &db.SecretRef{Name: ref.Name, Namespace: ref.Namespace}
}
