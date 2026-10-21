// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// validNicoSpec is the shape the nico lane bootstraps, with the simulator's
// own fixed identifiers. It matches e2e/bootstrap/run-nico.sh.
func validNicoSpec(releasesDir string) NicoSiteSpec {
	return NicoSiteSpec{
		ClusterName:            "nvke-e2e-nico",
		OrgID:                  "novxwn552ssc",
		Endpoint:               "http://172.28.0.1:18080",
		SiteID:                 "00000000-0000-0000-0000-000000000001",
		VPCID:                  "00000000-0000-0000-0000-000000000002",
		SubnetIDs:              []string{"00000000-0000-0000-0000-000000000003"},
		SSHKeyGroupIDs:         []string{"00000000-0000-0000-0000-000000000004"},
		InstanceTypePreference: []string{"00000000-0000-0000-0000-000000000005"},
		NodeCount:              1,
		Release:                "nvke-local-dev-kindest-arm64",
		ConfigProfile:          "dev-only-empty",
		ReleasesDir:            releasesDir,
		K8sVersion:             "1.34.8",
		CNIName:                "ovn-kubernetes",
		CNIVersion:             "2.4.2",
		PodCIDR:                "192.168.0.0/16",
		ServiceCIDR:            "10.96.0.0/12",
		SystemReservedMemory:   "1Gi",
		TrustDomain:            "nvkectl-e2e.example.test",
		SpireExternalAddress:   "172.28.0.1",
	}
}

// releasesFixture copies the release pair the spec names into a directory of
// its own, as the lane does: only the pair, not every fixture release.
func releasesFixture(t *testing.T, release string) string {
	t.Helper()
	root, err := RepoRoot()
	require.NoError(t, err)
	src := filepath.Join(root, "controller", "test", "fixtures", "releases")

	dir := t.TempDir()
	for _, kind := range []string{"machineimage", "clusterrelease"} {
		name := release + "-" + kind + ".yaml"
		content, err := os.ReadFile(filepath.Join(src, name))
		require.NoError(t, err, "the lane copies this fixture; it must be there")
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), content, 0o600))
	}
	return dir
}

// TestNicoSiteSpec_TheRealLoaderAcceptsIt is the point of rendering the spec
// from a struct: the site spec schema is checked here, in the merge gate, and
// not twenty minutes into a lane.
func TestNicoSiteSpec_TheRealLoaderAcceptsIt(t *testing.T) {
	spec := validNicoSpec(releasesFixture(t, "nvke-local-dev-kindest-arm64"))

	// nico-sim serves http and takes any token, as the lane's own environment
	// says; the loader refuses both without being told.
	tokenFile := filepath.Join(t.TempDir(), "nico.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("e2e-nico-token"), 0o600))
	t.Setenv("NVKECTL_NICO_CREDENTIALS", tokenFile)
	t.Setenv("NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT", "1")
	t.Setenv("NVKECTL_NODE_USER", "nvke-e2e")
	t.Setenv("NVKECTL_NODE_PASSWORD", "e2e-node-password")

	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	require.NoError(t, spec.WriteTo(path))

	loaded, err := sitespec.Load(path)
	require.NoError(t, err, "the rendered spec must satisfy the loader the command uses")

	assert.Equal(t, spec.ClusterName, loaded.ClusterName)
	assert.True(t, loaded.IsNico(), "the spec must select the nico provider")
	require.NotNil(t, loaded.Infra.Nico)
	assert.Equal(t, spec.OrgID, loaded.Infra.Nico.OrgID)
	assert.Equal(t, spec.NodeCount, loaded.Infra.Nico.ControlPlane.Count)
	assert.Equal(t, spec.Release, loaded.Infra.Nico.ControlPlane.Release)
	assert.Equal(t, spec.TrustDomain, loaded.Spire.TrustDomain)
	assert.Equal(t, spec.SpireExternalAddress, loaded.Spire.ExternalAddress)
	assert.Equal(t, spec.K8sVersion, loaded.Cluster.K8sVersion)
	require.NotNil(t, loaded.Infra.Nico.Remote)
	assert.Equal(t, spec.Endpoint, loaded.Infra.Nico.Remote.Endpoint)
}

// TestNicoSiteSpec_VIPReachesTheLoader: a multi-node nico site needs a VIP,
// and it has to arrive as the cluster endpoint rather than being dropped by
// the template.
func TestNicoSiteSpec_VIPReachesTheLoader(t *testing.T) {
	spec := validNicoSpec(releasesFixture(t, "nvke-local-dev-kindest-arm64"))
	spec.NodeCount = 3
	spec.ControlPlaneVIP = "172.26.1.1"

	tokenFile := filepath.Join(t.TempDir(), "nico.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("e2e-nico-token"), 0o600))
	t.Setenv("NVKECTL_NICO_CREDENTIALS", tokenFile)
	t.Setenv("NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT", "1")
	t.Setenv("NVKECTL_NODE_USER", "nvke-e2e")
	t.Setenv("NVKECTL_NODE_PASSWORD", "e2e-node-password")

	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	require.NoError(t, spec.WriteTo(path))

	loaded, err := sitespec.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "172.26.1.1", loaded.Cluster.ControlPlaneVIP)
	assert.Equal(t, 3, loaded.Infra.Nico.ControlPlane.Count)
}

// TestNicoSiteSpec_RefusesAnIncompleteSpec: a field the lane forgot is named,
// rather than reaching the loader as a malformed file.
func TestNicoSiteSpec_RefusesAnIncompleteSpec(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*NicoSiteSpec)
		wantErr string
	}{
		{"no cluster name", func(s *NicoSiteSpec) { s.ClusterName = "" }, "ClusterName"},
		{"no endpoint", func(s *NicoSiteSpec) { s.Endpoint = "" }, "Endpoint"},
		{"no subnets", func(s *NicoSiteSpec) { s.SubnetIDs = nil }, "SubnetIDs"},
		{"no ssh key groups", func(s *NicoSiteSpec) { s.SSHKeyGroupIDs = nil }, "SSHKeyGroupIDs"},
		{"no instance types", func(s *NicoSiteSpec) { s.InstanceTypePreference = nil }, "InstanceTypePreference"},
		{"no nodes", func(s *NicoSiteSpec) { s.NodeCount = 0 }, "NodeCount"},
		{"no spire address", func(s *NicoSiteSpec) { s.SpireExternalAddress = "" }, "SpireExternalAddress"},
		{"three nodes and no VIP", func(s *NicoSiteSpec) { s.NodeCount = 3 }, "needs a ControlPlaneVIP"},
		{
			"several at once are all named",
			func(s *NicoSiteSpec) { s.OrgID = ""; s.Release = "" },
			"OrgID, Release",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validNicoSpec("/tmp/releases")
			tt.mutate(&spec)
			_, err := spec.Render()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestNicoSiteSpec_OmitsWhatWasNotAsked: an empty optional field leaves the
// key out rather than writing an empty value the loader would read as a
// deliberate one.
func TestNicoSiteSpec_OmitsWhatWasNotAsked(t *testing.T) {
	spec := validNicoSpec("/tmp/releases")
	spec.ConfigProfile = ""
	spec.ControlPlaneVIP = ""

	rendered, err := spec.Render()
	require.NoError(t, err)
	assert.NotContains(t, rendered, "configProfile")
	assert.NotContains(t, rendered, "controlPlaneVIP")

	spec.ConfigProfile = "dev-only-empty"
	spec.ControlPlaneVIP = "172.26.1.1"
	rendered, err = spec.Render()
	require.NoError(t, err)
	assert.Contains(t, rendered, "configProfile: dev-only-empty")
	assert.Contains(t, rendered, "controlPlaneVIP: 172.26.1.1")
}

func TestFirstIPv4(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"one address", "172.28.0.1 ", "172.28.0.1", false},
		{"IPv6 first is skipped", "fd00::1 172.28.0.1 ", "172.28.0.1", false},
		{"nothing at all", "   ", "", true},
		{"only IPv6", "fd00::1 ", "", true},
		{"not an address", "not-an-address ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := firstIPv4(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
