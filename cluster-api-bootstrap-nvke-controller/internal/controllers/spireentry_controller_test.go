// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	bootstrapv1 "github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/api/v1alpha1"
)

// machineUID is a realistic Kubernetes UID: the workload ID derivation relies
// on it already being UUID-shaped.
const machineUID = "7d3f1c2a-9b6e-4f08-8a51-2c7d4e9f0b13"

func TestAgentSPIFFEIDPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		method   bootstrapv1.AttestationMethod
		identity agentIdentity
		want     string
		wantErr  error
	}{
		{
			name:     "tpm uses the EK public hash",
			method:   bootstrapv1.AttestationMethodTPM,
			identity: agentIdentity{HostID: machineUID, TPMEKPubHash: "d7a1f0"},
			want:     "/spire/agent/tpm/d7a1f0",
		},
		{
			name:     "tpm waits for the infrastructure provider to publish the hash",
			method:   bootstrapv1.AttestationMethodTPM,
			identity: agentIdentity{HostID: machineUID},
			wantErr:  errAgentIdentityPending,
		},
		{
			name:     "tpm ignores the DPU machine ID",
			method:   bootstrapv1.AttestationMethodTPM,
			identity: agentIdentity{HostID: machineUID, TPMEKPubHash: "d7a1f0", PrimaryDPUMachineID: "dpu-1"},
			want:     "/spire/agent/tpm/d7a1f0",
		},
		{
			name:     "dsxImds nests the DPU machine ID under the operator tenant",
			method:   bootstrapv1.AttestationMethodDSXIMDS,
			identity: agentIdentity{HostID: machineUID, PrimaryDPUMachineID: "dpu-1", DSXIMDSOperatorTenant: "nke"},
			want:     "/spire/agent/dsx_imds/tenant/nke/bm/dpu-1",
		},
		{
			name:     "dsxImds waits for the DPU machine ID",
			method:   bootstrapv1.AttestationMethodDSXIMDS,
			identity: agentIdentity{HostID: machineUID, DSXIMDSOperatorTenant: "nke"},
			wantErr:  errAgentIdentityPending,
		},
		{
			name:     "dsxImds without an operator tenant is a configuration error, not a wait",
			method:   bootstrapv1.AttestationMethodDSXIMDS,
			identity: agentIdentity{HostID: machineUID, PrimaryDPUMachineID: "dpu-1"},
			wantErr:  errAttestationMethodUnsupported,
		},
		{
			name:     "joinToken uses the host ID, which is the Machine UID",
			method:   bootstrapv1.AttestationMethodJoinToken,
			identity: agentIdentity{HostID: machineUID},
			want:     "/join-token-spire-agent/host-id/" + machineUID,
		},
		{
			name:     "joinToken without a host ID",
			method:   bootstrapv1.AttestationMethodJoinToken,
			identity: agentIdentity{},
			wantErr:  errAgentIdentityPending,
		},
		{
			name:     "an unknown method is rejected rather than defaulted",
			method:   bootstrapv1.AttestationMethod("ec2"),
			identity: agentIdentity{HostID: machineUID, TPMEKPubHash: "d7a1f0"},
			wantErr:  errAttestationMethodUnsupported,
		},
		{
			name:     "an empty method is rejected; the caller defaults it",
			method:   "",
			identity: agentIdentity{HostID: machineUID, TPMEKPubHash: "d7a1f0"},
			wantErr:  errAttestationMethodUnsupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := agentSPIFFEIDPath(tt.method, tt.identity)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestAgentSPIFFEIDPathMatchesNVKEController pins the three paths against the
// values the NVKE controller produces today. A SPIRE server parents on an
// exact string, so a drift here is silent: the entry is accepted and nothing
// ever matches it.
func TestAgentSPIFFEIDPathMatchesNVKEController(t *testing.T) {
	t.Parallel()

	tpm, err := agentSPIFFEIDPath(bootstrapv1.AttestationMethodTPM, agentIdentity{TPMEKPubHash: "test-hash"})
	require.NoError(t, err)
	assert.Equal(t, "spiffe://example.org/spire/agent/tpm/test-hash", spiffeIDFromPath("example.org", tpm))

	dsx, err := agentSPIFFEIDPath(bootstrapv1.AttestationMethodDSXIMDS, agentIdentity{
		PrimaryDPUMachineID:   "machine-1",
		DSXIMDSOperatorTenant: "nke",
	})
	require.NoError(t, err)
	assert.Equal(t, "spiffe://example.org/spire/agent/dsx_imds/tenant/nke/bm/machine-1", spiffeIDFromPath("example.org", dsx))

	joinToken, err := agentSPIFFEIDPath(bootstrapv1.AttestationMethodJoinToken, agentIdentity{HostID: machineUID})
	require.NoError(t, err)
	assert.Equal(t, "spiffe://example.org/join-token-spire-agent/host-id/"+machineUID, spiffeIDFromPath("example.org", joinToken))
}

func TestWorkloadSPIFFEID(t *testing.T) {
	t.Parallel()

	// The NVKE controller's unit test asserts this exact form for a node, with
	// the node UUID where the Machine UID is here.
	assert.Equal(t, "spiffe://example.org/host/"+machineUID, workloadSPIFFEID("example.org", machineUID))
}

func TestSpiffeIDFromPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		trustDomain string
		path        string
		want        string
	}{
		{name: "an absolute path is joined as is", trustDomain: "example.org", path: "/spire/agent/tpm/h", want: "spiffe://example.org/spire/agent/tpm/h"},
		{name: "a relative path gains a separator", trustDomain: "example.org", path: "spire/agent/tpm/h", want: "spiffe://example.org/spire/agent/tpm/h"},
		{name: "an empty path gains a separator", trustDomain: "example.org", path: "", want: "spiffe://example.org/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, spiffeIDFromPath(tt.trustDomain, tt.path))
		})
	}
}

// TestMachineHostID documents the input choice: the entry is keyed on the
// owning Machine's UID, not on its name, because a ClusterStaticEntry is
// cluster-scoped and two namespaces may hold a Machine of the same name.
func TestMachineHostID(t *testing.T) {
	t.Parallel()

	first := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-0", Namespace: "blue", UID: types.UID(machineUID),
	}}
	second := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-0", Namespace: "green", UID: types.UID("0f0c1d2e-3a4b-5c6d-7e8f-9a0b1c2d3e4f"),
	}}

	assert.Equal(t, machineUID, machineHostID(first))
	assert.NotEqual(t, machineHostID(first), machineHostID(second))
	assert.NotEqual(t, spireEntryName(machineHostID(first)), spireEntryName(machineHostID(second)))
	assert.Equal(t, "capi-host-"+machineUID, spireEntryName(machineHostID(first)))
}

// TestSpireEntryNameIsNotTheNVKEControllers keeps the two controllers off one
// object. A shared name makes two field managers fight over every field.
func TestSpireEntryNameIsNotTheNVKEControllers(t *testing.T) {
	t.Parallel()

	assert.NotEqual(t, "host-"+machineUID, spireEntryName(machineUID))
}

func TestIsNvkeBootstrapConfigRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  clusterv1.ContractVersionedObjectReference
		want bool
	}{
		{
			name: "this provider's config",
			ref:  clusterv1.ContractVersionedObjectReference{APIGroup: bootstrapv1.GroupName, Kind: "NvkeConfig", Name: "worker-0"},
			want: true,
		},
		{
			name: "another bootstrap provider in the same group",
			ref:  clusterv1.ContractVersionedObjectReference{APIGroup: bootstrapv1.GroupName, Kind: "KubeadmConfig", Name: "worker-0"},
			want: false,
		},
		{
			name: "the same kind in another group",
			ref:  clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.example.com", Kind: "NvkeConfig", Name: "worker-0"},
			want: false,
		},
		{
			name: "no reference at all, for a Machine bootstrapped from a bare data secret",
			ref:  clusterv1.ContractVersionedObjectReference{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, isNvkeBootstrapConfigRef(tt.ref))
			machine := &clusterv1.Machine{Spec: clusterv1.MachineSpec{Bootstrap: clusterv1.Bootstrap{ConfigRef: tt.ref}}}
			assert.Equal(t, tt.want, hasNvkeBootstrapConfig(machine))
		})
	}
}

func TestSpireClassNameDefaults(t *testing.T) {
	t.Parallel()

	assert.Equal(t, defaultSpireClassName, (&SpireEntryReconciler{}).spireClassName())
	assert.Equal(t, "other-class", (&SpireEntryReconciler{SpireClassName: "other-class"}).spireClassName())
}
