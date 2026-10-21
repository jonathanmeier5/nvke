// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e_bootstrap

package bootstraptest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The identifiers nico-sim seeds for its container site. They are the
// simulator's own, shared with controller/hack/create-local-nico-cluster.sh
// and e2e/bootstrap/run-nico.sh.
const (
	nicoSiteID         = "00000000-0000-0000-0000-000000000001"
	nicoVPCID          = "00000000-0000-0000-0000-000000000002"
	nicoSubnetID       = "00000000-0000-0000-0000-000000000003"
	nicoSSHKeyGroupID  = "00000000-0000-0000-0000-000000000004"
	nicoInstanceTypeID = "00000000-0000-0000-0000-000000000005"
)

// TestBootstrapSite_Nico builds a site cluster whose control-plane machines
// CAPNICo allocates from a NICo site, and states what the run produced.
//
// It takes a prepared host: the node network, nico-sim and the profiles image
// are still stood up by e2e/bootstrap/run-nico.sh, which is the next piece to
// move here. What this owns is the site spec, the command and the cluster
// assertions. The prerequisites are checked up front and named, so an
// unprepared host says so in a second rather than failing somewhere inside
// the bootstrap.
func TestBootstrapSite_Nico(t *testing.T) {
	cfg := GoNicoLaneConfig()
	require.NoError(t, cfg.Validate())

	RequireRoot(t)
	RequireTools(t, ContainerCLI, "kind", "kubectl", "helm", "clusterctl", "iptables", "ip")
	RequireEnv(t,
		"NVKECTL_NVKE_CONTROLLER_IMAGE",
		// The lane sends this token to nico-sim; run-nico.sh writes the file.
		"NVKECTL_NICO_CREDENTIALS",
		// The login the controller puts on each node.
		"NVKECTL_NODE_USER", "NVKECTL_NODE_PASSWORD",
	)
	RequireLocalImage(t, os.Getenv("NVKECTL_NVKE_CONTROLLER_IMAGE"), "build it with `make -C cli images`")

	cliDir, err := CLIDir()
	require.NoError(t, err)
	nvkectl, err := FindNvkectl(cliDir)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.BootstrapTimeout+cfg.ReadyTimeout+10*time.Minute)
	defer cancel()

	// ── the prepared host ─────────────────────────────────────────────────
	gateway, err := GatewayOf(ctx, cfg.NodeNetwork)
	if err != nil {
		unmet(t, "the node network %s is not up: stand it up with `make -C cli test-e2e-bootstrap-nico` "+
			"or run e2e/bootstrap/run-nico.sh, which prepares nico-sim and the profiles image (%v)",
			cfg.NodeNetwork, err)
	}
	simPort := envOr("NVKECTL_E2E_NICO_SIM_PORT", "18080")
	simAddress := net.JoinHostPort(gateway, simPort)
	if err := WaitForTCP(ctx, simAddress, 10*time.Second); err != nil {
		unmet(t, "nico-sim is not answering at %s: it is still started by e2e/bootstrap/run-nico.sh (%v)",
			simAddress, err)
	}
	t.Logf("nico-sim answers at %s; the host is %s on %s", simAddress, gateway, cfg.NodeNetwork)

	// ── the site spec the operator would write ────────────────────────────
	release := envOr("NVKECTL_E2E_NICO_OS_PROFILE", "nvke-local-dev-kindest-arm64")
	spec := NicoSiteSpec{
		ClusterName:            cfg.ClusterName,
		OrgID:                  envOr("NVKECTL_E2E_NICO_ORG_ID", "novxwn552ssc"),
		Endpoint:               "http://" + simAddress,
		SiteID:                 nicoSiteID,
		VPCID:                  nicoVPCID,
		SubnetIDs:              []string{nicoSubnetID},
		SSHKeyGroupIDs:         []string{nicoSSHKeyGroupID},
		InstanceTypePreference: []string{nicoInstanceTypeID},
		NodeCount:              cfg.NodeCount,
		Release:                release,
		ConfigProfile:          envOr("NVKECTL_E2E_NICO_CONFIG_PROFILE", "dev-only-empty"),
		ReleasesDir:            releasesFixture(t, release),
		ControlPlaneVIP:        cfg.ControlPlaneVIP,
		K8sVersion:             "1.34.8",
		CNIName:                "ovn-kubernetes",
		CNIVersion:             "2.4.2",
		PodCIDR:                "192.168.0.0/16",
		ServiceCIDR:            "10.96.0.0/12",
		SystemReservedMemory:   "1Gi",
		TrustDomain:            cfg.SpireTrustDomain,
		// Nodes reach SPIRE at the host's address on their own network.
		SpireExternalAddress: gateway,
	}

	work := t.TempDir()
	siteSpec := filepath.Join(work, "sitespec.yaml")
	require.NoError(t, spec.WriteTo(siteSpec))

	// ── the command under test ────────────────────────────────────────────
	kubeconfig := filepath.Join(work, "site.kubeconfig")
	opts := BootstrapOptions{
		Binary:               nvkectl,
		SiteSpec:             siteSpec,
		KubeconfigOut:        kubeconfig,
		EncryptionKeyOut:     filepath.Join(work, "etcd-encryption-key.json"),
		DiagnosticsDir:       filepath.Join(cliDir, "cluster-diagnostics"),
		Timeout:              cfg.BootstrapTimeout,
		KeepBootstrapCluster: true,
		// nico-sim serves http, which the bootstrap refuses for a NICo it
		// sends a token to unless told.
		Env: []string{"NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT=1"},
	}
	bootstrapCluster := envOr("NVKECTL_BOOTSTRAP_CLUSTER_NAME", "nvkectl-bootstrap")
	t.Cleanup(func() { deleteKindCluster(context.WithoutCancel(ctx), t, bootstrapCluster) })

	b, err := StartBootstrap(ctx, opts, t.Logf)
	require.NoError(t, err)
	t.Cleanup(b.Stop)

	// ── bridge the networks, which has to happen during the run ───────────
	milestoneCtx, cancelMilestone := context.WithTimeout(ctx, cfg.MilestoneTimeout)
	defer cancelMilestone()
	require.NoError(t, b.WaitForMilestone(milestoneCtx, MilestoneKindClusterReady, 2*time.Second),
		"the bootstrap never reported its Kind cluster ready, so there was nothing to bridge")

	bridges, err := AttachBridges(ctx, KindNetwork, cfg.NodeNetwork, cfg.ControlPlaneVIP != "", t.Logf)
	require.NoError(t, err, "without the bridge the controller cannot reach the machines CAPNICo allocates")
	t.Cleanup(func() { bridges.Detach(context.WithoutCancel(ctx)) })

	// ── what the run produced ─────────────────────────────────────────────
	if runErr := b.Wait(); runErr != nil {
		t.Fatalf("nvkectl bootstrap site failed: %v", runErr)
	}
	assert.Empty(t, MissingMilestones(b.Output(),
		MilestoneKindClusterReady, MilestoneControllerHealthy, MilestoneClusterProvisioned),
		"the command exited 0 without reporting every stage")
	assert.Contains(t, b.Output(), "installing CAPNICo",
		"a nico site's machines come from CAPNICo; the run never installed it")

	cluster, err := OpenCluster(kubeconfig)
	require.NoError(t, err, "bootstrap site must leave a usable kubeconfig behind")

	version, err := cluster.Version()
	require.NoError(t, err, "the cluster must answer, not just be described by a file")
	t.Logf("the site cluster at %s answers as Kubernetes %s", cluster.Server, version)

	if cfg.ControlPlaneVIP != "" {
		assert.Equal(t, fmt.Sprintf("https://%s:6443", cfg.ControlPlaneVIP), cluster.Server,
			"with a VIP the written kubeconfig must point at it, not at a single machine")
	}

	states, err := cluster.WaitNodesReady(ctx, cfg.NodeCount, cfg.ReadyTimeout, 5*time.Second)
	require.NoError(t, err)
	t.Logf("the cluster nvkectl built holds %s", describeNodes(states))
}
