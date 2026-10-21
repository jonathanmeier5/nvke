// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaneConfigValidate(t *testing.T) {
	valid := LaneConfig{ClusterName: "nke-e2e", NodeCount: 3, ControlPlaneVIP: "172.26.1.1"}

	tests := []struct {
		name    string
		mutate  func(*LaneConfig)
		wantErr string
	}{
		{"a three-node site with a VIP", func(*LaneConfig) {}, ""},
		{"one node needs no VIP", func(c *LaneConfig) { c.NodeCount = 1; c.ControlPlaneVIP = "" }, ""},
		{"no cluster name", func(c *LaneConfig) { c.ClusterName = "" }, "no cluster name"},
		{"no nodes", func(c *LaneConfig) { c.NodeCount = 0 }, "at least one"},
		{"a multi-node site without a VIP", func(c *LaneConfig) { c.ControlPlaneVIP = "" }, "needs a control-plane VIP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestLaneConfigsMatchTheShellDefaults: the Go lanes and the shell lanes read
// the same variables, so one environment drives both and a value set for a
// debugging run does not apply to only one of them.
func TestLaneConfigsMatchTheShellDefaults(t *testing.T) {
	t.Run("provisioned", func(t *testing.T) {
		cfg := ProvisionedLaneConfig()
		assert.Equal(t, "nke-e2e", cfg.ClusterName)
		assert.Equal(t, 3, cfg.NodeCount)
		assert.Equal(t, "172.26.1.1", cfg.ControlPlaneVIP)
		assert.Equal(t, "nvkectl-e2e-nodes", cfg.NodeNetwork)
		assert.Equal(t, 15*time.Minute, cfg.BootstrapTimeout)
		assert.NoError(t, cfg.Validate())
	})

	t.Run("nico", func(t *testing.T) {
		cfg := NicoLaneConfig()
		assert.Equal(t, "nvke-e2e-nico", cfg.ClusterName)
		assert.Equal(t, "nvkectl-e2e-nico-nodes", cfg.NodeNetwork)
		assert.Equal(t, 25*time.Minute, cfg.BootstrapTimeout)
		assert.NoError(t, cfg.Validate())
	})
}

// TestLaneConfigReadsTheEnvironment: the knobs are the shell lanes' variable
// names, and an explicitly empty VIP turns the VIP off rather than falling
// back to the default.
func TestLaneConfigReadsTheEnvironment(t *testing.T) {
	t.Setenv("NVKECTL_E2E_NODES", "1")
	t.Setenv("NVKECTL_E2E_CONTROL_PLANE_VIP", "")
	t.Setenv("NVKECTL_E2E_SPIRE_TRUST_DOMAIN", "lane.example.test")
	t.Setenv("NVKECTL_E2E_BOOTSTRAP_TIMEOUT", "3m")

	cfg := ProvisionedLaneConfig()
	assert.Equal(t, 1, cfg.NodeCount)
	assert.Empty(t, cfg.ControlPlaneVIP, "an explicitly empty VIP must not fall back to the default")
	assert.Equal(t, "lane.example.test", cfg.SpireTrustDomain)
	assert.Equal(t, 3*time.Minute, cfg.BootstrapTimeout)
	assert.NoError(t, cfg.Validate(), "one node without a VIP is allowed")
}

// TestEnvHelpersIgnoreUnusableValues: a mistyped value falls back to the
// default rather than standing the lane up with zero nodes or no timeout.
func TestEnvHelpersIgnoreUnusableValues(t *testing.T) {
	t.Setenv("NVKECTL_E2E_NODES", "three")
	t.Setenv("NVKECTL_E2E_BOOTSTRAP_TIMEOUT", "quarter of an hour")
	cfg := ProvisionedLaneConfig()
	assert.Equal(t, 3, cfg.NodeCount)
	assert.Equal(t, 15*time.Minute, cfg.BootstrapTimeout)

	t.Setenv("NVKECTL_E2E_NODES", "0")
	t.Setenv("NVKECTL_E2E_BOOTSTRAP_TIMEOUT", "-5m")
	cfg = ProvisionedLaneConfig()
	assert.Equal(t, 3, cfg.NodeCount, "a zero node count is not a usable override")
	assert.Equal(t, 15*time.Minute, cfg.BootstrapTimeout, "a negative timeout is not a usable override")
}

// sprintf keeps the Logf-shaped test sinks readable.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// fakeTB records what the prerequisite helpers did, so their skip-or-fail
// behaviour can be tested without skipping the test that checks it.
type fakeTB struct {
	skipped string
	failed  string
}

func (f *fakeTB) Helper()                           {}
func (f *fakeTB) Logf(string, ...any)               {}
func (f *fakeTB) Skipf(format string, args ...any)  { f.skipped = sprintf(format, args...) }
func (f *fakeTB) Fatalf(format string, args ...any) { f.failed = sprintf(format, args...) }

// TestUnmetPrerequisiteSkipsUnlessRequired: a lane that quietly skips every
// run looks exactly like one that passes, so CI sets the variable that turns
// the skip into a failure.
func TestUnmetPrerequisiteSkipsUnlessRequired(t *testing.T) {
	t.Run("skips by default", func(t *testing.T) {
		t.Setenv(RequireEnvVar, "")
		tb := &fakeTB{}
		RequireTools(tb, "a-tool-that-is-not-installed-anywhere")
		assert.Contains(t, tb.skipped, "a-tool-that-is-not-installed-anywhere")
		assert.Empty(t, tb.failed)
	})

	t.Run("fails when the lane is required", func(t *testing.T) {
		t.Setenv(RequireEnvVar, "1")
		tb := &fakeTB{}
		RequireTools(tb, "a-tool-that-is-not-installed-anywhere")
		assert.Contains(t, tb.failed, "a-tool-that-is-not-installed-anywhere")
		assert.Empty(t, tb.skipped)
	})

	t.Run("a tool that is there is no prerequisite at all", func(t *testing.T) {
		t.Setenv(RequireEnvVar, "1")
		tb := &fakeTB{}
		RequireTools(tb, "sh")
		assert.Empty(t, tb.failed)
		assert.Empty(t, tb.skipped)
	})

	t.Run("environment variables", func(t *testing.T) {
		t.Setenv(RequireEnvVar, "")
		t.Setenv("NVKECTL_E2E_A_VARIABLE_NOBODY_SET", "")
		tb := &fakeTB{}
		RequireEnv(tb, "NVKECTL_E2E_A_VARIABLE_NOBODY_SET")
		assert.Contains(t, tb.skipped, "NVKECTL_E2E_A_VARIABLE_NOBODY_SET")
	})
}

// TestCLIDirResolvesTheModule: the lanes find the nvkectl binary and the
// repository fixtures relative to this, so a wrong answer is a confusing
// failure much later.
func TestCLIDirResolvesTheModule(t *testing.T) {
	dir, err := CLIDir()
	require.NoError(t, err)
	assert.True(t, len(dir) > 0)

	root, err := RepoRoot()
	require.NoError(t, err)
	assert.NotEqual(t, dir, root)
}

// TestGoLaneConfigsStandUpWithoutABGPSpeaker: by default the Go lanes ask for
// a shape they can reach, and an explicit request in the environment still
// wins.
func TestGoLaneConfigsStandUpWithoutABGPSpeaker(t *testing.T) {
	t.Run("the default needs no VIP", func(t *testing.T) {
		cfg := GoProvisionedLaneConfig()
		assert.Equal(t, 1, cfg.NodeCount)
		assert.Empty(t, cfg.ControlPlaneVIP)
		require.NoError(t, cfg.Validate())
	})

	t.Run("an explicit shape wins", func(t *testing.T) {
		t.Setenv("NVKECTL_E2E_NODES", "3")
		t.Setenv("NVKECTL_E2E_CONTROL_PLANE_VIP", "172.26.1.1")
		cfg := GoProvisionedLaneConfig()
		assert.Equal(t, 3, cfg.NodeCount)
		assert.Equal(t, "172.26.1.1", cfg.ControlPlaneVIP)
		require.NoError(t, cfg.Validate())
	})

	t.Run("asking for three nodes without a VIP is refused, not quietly narrowed", func(t *testing.T) {
		t.Setenv("NVKECTL_E2E_NODES", "3")
		t.Setenv("NVKECTL_E2E_CONTROL_PLANE_VIP", "")
		require.Error(t, GoProvisionedLaneConfig().Validate())
	})

	t.Run("nico", func(t *testing.T) {
		cfg := GoNicoLaneConfig()
		assert.Equal(t, 1, cfg.NodeCount)
		assert.Empty(t, cfg.ControlPlaneVIP)
		require.NoError(t, cfg.Validate())
	})
}
