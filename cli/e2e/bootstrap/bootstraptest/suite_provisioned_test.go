// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e_bootstrap

package bootstraptest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/e2e/bootstrap/substrate"
)

// TestBootstrapSite_Provisioned builds a site cluster on container nodes that
// stand in for pre-provisioned hardware, and states what the run produced.
//
// This is the Go half of e2e/bootstrap/run.sh: the fixture, the command and
// the cluster assertions. What run.sh still owns, and this does not, is the
// BGP speaker a control-plane VIP needs and the deeper checks on SPIRE, etcd
// encryption and the ingress path.
func TestBootstrapSite_Provisioned(t *testing.T) {
	cfg := GoProvisionedLaneConfig()
	require.NoError(t, cfg.Validate())

	RequireRoot(t)
	RequireTools(t, ContainerCLI, "kind", "kubectl", "helm", "clusterctl", "iptables", "ip")
	RequireEnv(t, "NVKECTL_NVKE_CONTROLLER_IMAGE")
	RequireLocalImage(t, os.Getenv("NVKECTL_NVKE_CONTROLLER_IMAGE"),
		"build it with `make -C cli images`")

	cliDir, err := CLIDir()
	require.NoError(t, err)
	nvkectl, err := FindNvkectl(cliDir)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.BootstrapTimeout+cfg.ReadyTimeout+10*time.Minute)
	defer cancel()

	// ── the nodes the site is built on ────────────────────────────────────
	subCfg := substrate.DefaultConfig()
	subCfg.NodeCount = cfg.NodeCount
	subCfg.Network = cfg.NodeNetwork
	subCfg.SpireTrustDomain = cfg.SpireTrustDomain
	RequireLocalImage(t, subCfg.BaseImage, "build it with `make -C cli ensure-capd-image`")

	t.Logf("starting %d node container(s) on %s", subCfg.NodeCount, subCfg.Network)
	sub, err := substrate.Start(ctx, subCfg, t.Logf)
	require.NoError(t, err, "the node fixture must come up before the bootstrap can use it")
	t.Cleanup(func() { sub.Stop(context.WithoutCancel(ctx)) })
	require.NoError(t, sub.ReadEKPubhashes(ctx), "every node must report a TPM EK thumbprint")

	// ── the site spec the operator would write ────────────────────────────
	work := t.TempDir()
	hostsCSV := filepath.Join(work, "hosts.csv")
	require.NoError(t, writeHostsCSV(hostsCSV, sub.Nodes()))

	caCert, caKey, err := substrate.EnsureBootstrapCA(subCfg.CADir)
	require.NoError(t, err)

	siteSpec := filepath.Join(work, "sitespec.yaml")
	require.NoError(t, generateSiteSpec(ctx, t, nvkectl, cfg, hostsCSV, siteSpec))

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
		Env: []string{
			"NVKECTL_SPIRE_CA_CERT_FILE=" + caCert,
			"NVKECTL_SPIRE_CA_KEY_FILE=" + caKey,
		},
	}
	bootstrapCluster := envOr("NVKECTL_BOOTSTRAP_CLUSTER_NAME", "nvkectl-bootstrap")
	t.Cleanup(func() { deleteKindCluster(context.WithoutCancel(ctx), t, bootstrapCluster) })

	b, err := StartBootstrap(ctx, opts, t.Logf)
	require.NoError(t, err)
	t.Cleanup(b.Stop)

	// ── bridge the networks, which has to happen during the run ───────────
	//
	// The node containers came up before the bootstrap cluster existed, so the
	// two sit on different bridges and nerdctl's firewall drops forwards
	// between them. Both directions are needed: the controller dials
	// nvke-agent, and each node's spire-agent dials the SPIRE server on the
	// bootstrap cluster's NodePort.
	milestoneCtx, cancelMilestone := context.WithTimeout(ctx, cfg.MilestoneTimeout)
	defer cancelMilestone()
	require.NoError(t, b.WaitForMilestone(milestoneCtx, MilestoneKindClusterReady, 2*time.Second),
		"the bootstrap never reported its Kind cluster ready, so there was nothing to bridge")

	bridges, err := AttachBridges(ctx, KindNetwork, cfg.NodeNetwork, cfg.ControlPlaneVIP != "", t.Logf)
	require.NoError(t, err, "without the bridge the controller cannot reach the nodes")
	t.Cleanup(func() { bridges.Detach(context.WithoutCancel(ctx)) })
	require.NoError(t, bridges.AddAddress(ctx, subCfg.SpireServerAddress+"/32"),
		"the agents were built to find SPIRE at this address")

	// ── what the run produced ─────────────────────────────────────────────
	runErr := b.Wait()
	if runErr != nil {
		t.Fatalf("nvkectl bootstrap site failed: %v", runErr)
	}
	assert.Empty(t, MissingMilestones(b.Output(),
		MilestoneKindClusterReady, MilestoneControllerHealthy, MilestoneClusterProvisioned),
		"the command exited 0 without reporting every stage")

	cluster, err := OpenCluster(kubeconfig)
	require.NoError(t, err, "bootstrap site must leave a usable kubeconfig behind")

	version, err := cluster.Version()
	require.NoError(t, err, "the cluster must answer, not just be described by a file")
	t.Logf("the site cluster at %s answers as Kubernetes %s", cluster.Server, version)

	if cfg.ControlPlaneVIP != "" {
		assert.Equal(t, fmt.Sprintf("https://%s:6443", cfg.ControlPlaneVIP), cluster.Server,
			"with a VIP the written kubeconfig must point at it, not at a single node")
	}

	states, err := cluster.WaitNodesReady(ctx, cfg.NodeCount, cfg.ReadyTimeout, 5*time.Second)
	require.NoError(t, err)
	t.Logf("the cluster nvkectl built holds %s", describeNodes(states))
}

// writeHostsCSV writes the node inventory `nvkectl generate sitespec --nodes`
// reads. The thumbprints cannot be committed: a TPM endorsement key is created
// with the container, so they are whatever this run produced.
func writeHostsCSV(path string, nodes []substrate.Node) error {
	var b strings.Builder
	b.WriteString("name,ip,tpm_ek_thumbprint\n")
	for _, n := range nodes {
		if n.EKPubhash == "" {
			return fmt.Errorf("node %s reported no TPM EK thumbprint", n.Name)
		}
		fmt.Fprintf(&b, "%s,%s,%s\n", n.Name, n.IP, n.EKPubhash)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("writing the node inventory to %s: %w", path, err)
	}
	return nil
}

// generateSiteSpec runs `nvkectl generate sitespec`, so the lane exercises the
// command an operator uses rather than a fixture kept in step by hand.
func generateSiteSpec(ctx context.Context, t *testing.T, nvkectl string, cfg LaneConfig, hostsCSV, out string) error {
	t.Helper()
	args := []string{
		"generate", "sitespec", cfg.ClusterName,
		"--nodes", hostsCSV,
		"--set", "cluster.k8sVersion=1.34.8",
		"--set", "cluster.cniName=ovn-kubernetes",
		"--set", "cluster.cniVersion=2.4.2",
		"--set", "cluster.podCidr=192.168.0.0/16",
		"--set", "cluster.serviceCidr=10.96.0.0/12",
		"--set", "cluster.kubeletConfig.systemReservedMemory=512Mi",
		"--set", "spire.trustDomain=" + cfg.SpireTrustDomain,
		"--set", "spire.upstreamCA=true",
	}
	if cfg.ControlPlaneVIP != "" {
		args = append(args, "--set", "cluster.controlPlaneVIP="+cfg.ControlPlaneVIP)
	}
	t.Logf("generating the site spec: %s %s", nvkectl, strings.Join(args, " "))
	rendered, err := exec.CommandContext(ctx, nvkectl, args...).Output()
	if err != nil {
		return fmt.Errorf("nvkectl generate sitespec: %w", err)
	}
	if len(rendered) == 0 {
		return fmt.Errorf("nvkectl generate sitespec produced an empty site spec")
	}
	if err := os.WriteFile(out, rendered, 0o600); err != nil {
		return fmt.Errorf("writing the site spec to %s: %w", out, err)
	}
	return nil
}

// deleteKindCluster removes the bootstrap cluster the run was told to keep.
// The lane asks for --keep-bootstrap-cluster so it can be read afterwards,
// which makes deleting it the lane's job.
func deleteKindCluster(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "kind", "delete", "cluster", "--name", name)
	cmd.Env = append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER="+ContainerCLI)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("could not delete the bootstrap kind cluster %s: %v\n%s", name, err, strings.TrimSpace(string(out)))
	}
}
