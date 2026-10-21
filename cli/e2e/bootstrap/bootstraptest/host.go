// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RequireEnvVar makes an unmet prerequisite a failure instead of a skip. CI
// sets it: a lane that quietly skips every run looks exactly like a lane that
// passes, and nothing else notices that it stopped testing anything.
const RequireEnvVar = "NVKECTL_E2E_REQUIRE"

// TB is the part of *testing.T this package uses. Taking an interface keeps
// the testing package out of the library's imports.
type TB interface {
	Helper()
	Logf(format string, args ...any)
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// unmet reports an unmet prerequisite: a skip normally, a failure when
// RequireEnvVar is set.
func unmet(tb TB, format string, args ...any) {
	tb.Helper()
	if os.Getenv(RequireEnvVar) == "1" {
		tb.Fatalf("%s (%s=1)", fmt.Sprintf(format, args...), RequireEnvVar)
		// (*testing.T).Fatalf does not return, but TB is an interface and a
		// caller's own implementation may: do not fall through to the skip.
		return
	}
	tb.Skipf("%s (set %s=1 to make this a failure)", fmt.Sprintf(format, args...), RequireEnvVar)
}

// RequireTools checks that every named tool is on PATH.
func RequireTools(tb TB, tools ...string) {
	tb.Helper()
	var missing []string
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		unmet(tb, "this lane needs %s on PATH", strings.Join(missing, ", "))
	}
}

// RequireRoot checks that the test runs as root. The lanes write iptables
// rules and put addresses on bridges, so they do.
func RequireRoot(tb TB) {
	tb.Helper()
	if os.Geteuid() != 0 {
		unmet(tb, "this lane writes iptables rules and bridge addresses, so it needs root: run it under sudo -E")
	}
}

// RequireEnv checks that every named environment variable is set and not empty.
func RequireEnv(tb TB, names ...string) {
	tb.Helper()
	var missing []string
	for _, name := range names {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		unmet(tb, "this lane needs %s set", strings.Join(missing, ", "))
	}
}

// RequireLocalImage checks that an image is in the local image store. The
// lanes side-load rather than pull, so an image that was never built fails
// here, in a second, instead of part way into a bootstrap.
func RequireLocalImage(tb TB, image, howToBuild string) {
	tb.Helper()
	if image == "" {
		unmet(tb, "no image named; %s", howToBuild)
		return
	}
	cmd := exec.Command(ContainerCLI, "image", "inspect", image)
	if out, err := cmd.CombinedOutput(); err != nil {
		unmet(tb, "image %s is not in the local image store (%v: %s); %s",
			image, err, strings.TrimSpace(string(out)), howToBuild)
	}
}

// CLIDir is the cli module's directory, resolved from the package directory a
// test runs in.
func CLIDir() (string, error) {
	// go test runs in the package directory: cli/e2e/bootstrap/bootstraptest.
	dir, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		return "", fmt.Errorf("resolving the cli directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("%s does not look like the cli module: %w", dir, err)
	}
	return dir, nil
}

// RepoRoot is the repository root, the parent of the cli module.
func RepoRoot() (string, error) {
	cli, err := CLIDir()
	if err != nil {
		return "", err
	}
	return filepath.Dir(cli), nil
}

// LaneConfig is what both lanes need to know about the run. The defaults and
// the variable names match e2e/bootstrap/run.sh and run-nico.sh, so one
// environment drives the shell lanes and these tests the same way.
type LaneConfig struct {
	// ClusterName is the site cluster to build.
	ClusterName string
	// NodeCount is how many control-plane nodes the site has, and so how many
	// Ready nodes the finished cluster must hold.
	NodeCount int
	// ControlPlaneVIP is the API endpoint kube-vip advertises. Empty means
	// none, which only a one-node site may do.
	ControlPlaneVIP string
	// SpireTrustDomain must match what the node images were built with.
	SpireTrustDomain string
	// NodeNetwork is the nerdctl network the node containers run on.
	NodeNetwork string
	// BootstrapTimeout is the command's own --timeout.
	BootstrapTimeout time.Duration
	// ReadyTimeout bounds the wait for every node to report Ready after the
	// command returns. nvkectl returns when the controller reports the nodes
	// provisioned, which can be before the last one's CNI settles.
	ReadyTimeout time.Duration
	// MilestoneTimeout bounds the wait for a milestone during the run.
	MilestoneTimeout time.Duration
}

// ProvisionedLaneConfig reads the provisioned lane's knobs, matching run.sh.
func ProvisionedLaneConfig() LaneConfig {
	return LaneConfig{
		ClusterName:      envOr("NVKECTL_E2E_CLUSTER_NAME", "nke-e2e"),
		NodeCount:        envInt("NVKECTL_E2E_NODES", 3),
		ControlPlaneVIP:  envOrKeepEmpty("NVKECTL_E2E_CONTROL_PLANE_VIP", "172.26.1.1"),
		SpireTrustDomain: envOr("NVKECTL_E2E_SPIRE_TRUST_DOMAIN", "nvkectl-e2e.example.test"),
		NodeNetwork:      envOr("NVKECTL_E2E_NETWORK", "nvkectl-e2e-nodes"),
		BootstrapTimeout: envDuration("NVKECTL_E2E_BOOTSTRAP_TIMEOUT", 15*time.Minute),
		ReadyTimeout:     envDuration("NVKECTL_E2E_NODE_READY_TIMEOUT", 10*time.Minute),
		MilestoneTimeout: envDuration("NVKECTL_E2E_ATTACH_TIMEOUT", 20*time.Minute),
	}
}

// NicoLaneConfig reads the nico lane's knobs, matching run-nico.sh.
func NicoLaneConfig() LaneConfig {
	return LaneConfig{
		ClusterName:      envOr("NVKECTL_E2E_NICO_CLUSTER_NAME", "nvke-e2e-nico"),
		NodeCount:        envInt("NVKECTL_E2E_NICO_NODES", 1),
		ControlPlaneVIP:  envOrKeepEmpty("NVKECTL_E2E_CONTROL_PLANE_VIP", "172.26.1.1"),
		SpireTrustDomain: envOr("NVKECTL_E2E_SPIRE_TRUST_DOMAIN", "nvkectl-e2e.example.test"),
		NodeNetwork:      envOr("NVKECTL_E2E_NICO_NETWORK", "nvkectl-e2e-nico-nodes"),
		BootstrapTimeout: envDuration("NVKECTL_E2E_BOOTSTRAP_TIMEOUT", 25*time.Minute),
		ReadyTimeout:     envDuration("NVKECTL_E2E_NODE_READY_TIMEOUT", 10*time.Minute),
		MilestoneTimeout: envDuration("NVKECTL_E2E_ATTACH_TIMEOUT", 20*time.Minute),
	}
}

// Validate reports a configuration the lane cannot run.
func (c LaneConfig) Validate() error {
	switch {
	case c.ClusterName == "":
		return fmt.Errorf("the lane has no cluster name")
	case c.NodeCount < 1:
		return fmt.Errorf("the lane wants %d node(s); it needs at least one", c.NodeCount)
	case c.ControlPlaneVIP == "" && c.NodeCount > 1:
		return fmt.Errorf("a %d-node control plane needs a control-plane VIP; only a one-node site may go without",
			c.NodeCount)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envOrKeepEmpty differs from envOr in honouring an explicitly empty value, so
// a lane can turn a feature off rather than only change it.
func envOrKeepEmpty(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(os.Getenv(key))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// narrowToSingleNode returns c shaped so a Go lane can stand it up on its own:
// one control-plane node and no control-plane VIP.
//
// A VIP is advertised by kube-vip over BGP and only becomes reachable once a
// speaker on the host learns the /32. That speaker is still started by
// e2e/bootstrap/run.sh, so a Go lane that asked for a VIP would write a
// kubeconfig pointing at an address it cannot reach. One node needs no VIP,
// and the site spec allows exactly that.
//
// An explicit value in the environment wins: setting NVKECTL_E2E_NODES or
// NVKECTL_E2E_CONTROL_PLANE_VIP asks for a shape, and the lane honours it.
func narrowToSingleNode(c LaneConfig, nodeCountVar string) LaneConfig {
	if os.Getenv(nodeCountVar) == "" {
		c.NodeCount = 1
	}
	if _, ok := os.LookupEnv("NVKECTL_E2E_CONTROL_PLANE_VIP"); !ok {
		c.ControlPlaneVIP = ""
	}
	return c
}

// GoProvisionedLaneConfig is ProvisionedLaneConfig in the shape the Go
// provisioned lane stands up by default. See narrowToSingleNode.
func GoProvisionedLaneConfig() LaneConfig {
	return narrowToSingleNode(ProvisionedLaneConfig(), "NVKECTL_E2E_NODES")
}

// GoNicoLaneConfig is NicoLaneConfig in the shape the Go nico lane stands up
// by default. See narrowToSingleNode.
func GoNicoLaneConfig() LaneConfig {
	return narrowToSingleNode(NicoLaneConfig(), "NVKECTL_E2E_NICO_NODES")
}
