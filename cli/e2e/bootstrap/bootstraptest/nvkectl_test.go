// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBootstrapOptionsArgs: an unset option leaves its flag off, so the
// command's own default applies rather than this package restating it.
func TestBootstrapOptionsArgs(t *testing.T) {
	tests := []struct {
		name string
		opts BootstrapOptions
		want []string
	}{
		{
			"only what is required",
			BootstrapOptions{Binary: "nvkectl", SiteSpec: "/tmp/spec.yaml"},
			[]string{"bootstrap", "site", "-f", "/tmp/spec.yaml"},
		},
		{
			"every option the lanes set",
			BootstrapOptions{
				Binary:               "nvkectl",
				SiteSpec:             "/tmp/spec.yaml",
				KubeconfigOut:        "/tmp/site.kubeconfig",
				EncryptionKeyOut:     "/tmp/key.json",
				DiagnosticsDir:       "/tmp/diag",
				Timeout:              15 * time.Minute,
				KeepBootstrapCluster: true,
			},
			[]string{
				"bootstrap", "site", "-f", "/tmp/spec.yaml",
				"--keep-bootstrap-cluster",
				"--timeout", "15m0s",
				"--kubeconfig-out", "/tmp/site.kubeconfig",
				"--encryption-key-out", "/tmp/key.json",
				"--diagnostics-dir", "/tmp/diag",
			},
		},
		{
			"mTLS off",
			BootstrapOptions{Binary: "nvkectl", SiteSpec: "/tmp/spec.yaml", DisableMTLS: true},
			[]string{"bootstrap", "site", "-f", "/tmp/spec.yaml", "--disable-mtls"},
		},
		{
			"a zero timeout is not passed as 0s",
			BootstrapOptions{Binary: "nvkectl", SiteSpec: "/tmp/spec.yaml", Timeout: 0},
			[]string{"bootstrap", "site", "-f", "/tmp/spec.yaml"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.opts.args())
		})
	}
}

// TestBootstrapOptionsArgsTimeoutParses: the rendered --timeout is a value the
// command's own duration flag accepts.
func TestBootstrapOptionsArgsTimeoutParses(t *testing.T) {
	args := BootstrapOptions{Binary: "n", SiteSpec: "s", Timeout: 25 * time.Minute}.args()
	i := slicesIndex(args, "--timeout")
	require.GreaterOrEqual(t, i, 0, "args = %v, want a --timeout", args)
	require.Less(t, i+1, len(args))
	d, err := time.ParseDuration(args[i+1])
	require.NoError(t, err)
	assert.Equal(t, 25*time.Minute, d)
}

func TestBootstrapOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    BootstrapOptions
		wantErr string
	}{
		{"complete", BootstrapOptions{Binary: "nvkectl", SiteSpec: "s"}, ""},
		{"no binary", BootstrapOptions{SiteSpec: "s"}, "Binary is empty"},
		{"no site spec", BootstrapOptions{Binary: "nvkectl"}, "SiteSpec is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestMissingMilestones: the milestones are reported in the order asked for,
// so the message names the earliest stage the run did not reach first.
func TestMissingMilestones(t *testing.T) {
	complete := "bootstrap kind cluster ready\ncontroller deployment healthy\ncluster provisioned\n"
	all := []string{MilestoneKindClusterReady, MilestoneControllerHealthy, MilestoneClusterProvisioned}

	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"a complete run", complete, nil},
		{
			"exited before the controller came up", "bootstrap kind cluster ready\n",
			[]string{MilestoneControllerHealthy, MilestoneClusterProvisioned},
		},
		{
			"exited 0 without provisioning", "bootstrap kind cluster ready\ncontroller deployment healthy\n",
			[]string{MilestoneClusterProvisioned},
		},
		{"nothing at all", "", all},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MissingMilestones(tt.out, all...))
		})
	}
}

// TestLineWriter: output is logged a whole line at a time, however the reads
// happened to split it, and a trailing partial line is held rather than lost
// mid-word.
func TestLineWriter(t *testing.T) {
	var got []string
	w := newLineWriter(func(format string, args ...any) {
		got = append(got, strings.TrimSpace(sprintf(format, args...)))
	})

	for _, chunk := range []string{"first li", "ne\nsecond line\r\nthi", "rd line\n", "trailing, no newline"} {
		n, err := w.Write([]byte(chunk))
		require.NoError(t, err)
		assert.Equal(t, len(chunk), n, "Write must report every byte consumed")
	}
	assert.Equal(t, []string{"first line", "second line", "third line"}, got)
}

func TestFindNvkectl(t *testing.T) {
	dir := t.TempDir()
	cliDir := filepath.Join(dir, "cli")
	require.NoError(t, os.MkdirAll(filepath.Join(cliDir, "bin"), 0o755))
	binary := filepath.Join(cliDir, "bin", "nvkectl")

	t.Run("not built yet", func(t *testing.T) {
		t.Setenv("NVKECTL", "")
		_, err := FindNvkectl(cliDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "make -C cli build")
	})

	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755))

	t.Run("built", func(t *testing.T) {
		t.Setenv("NVKECTL", "")
		got, err := FindNvkectl(cliDir)
		require.NoError(t, err)
		assert.Equal(t, binary, got)
	})

	t.Run("NVKECTL wins", func(t *testing.T) {
		other := filepath.Join(dir, "other-nvkectl")
		require.NoError(t, os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755))
		t.Setenv("NVKECTL", other)
		got, err := FindNvkectl(cliDir)
		require.NoError(t, err)
		assert.Equal(t, other, got)
	})

	t.Run("a directory is not a binary", func(t *testing.T) {
		t.Setenv("NVKECTL", dir)
		_, err := FindNvkectl(cliDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not an executable file")
	})

	t.Run("a file that is not executable", func(t *testing.T) {
		plain := filepath.Join(dir, "plain")
		require.NoError(t, os.WriteFile(plain, []byte("x"), 0o644))
		t.Setenv("NVKECTL", plain)
		_, err := FindNvkectl(cliDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not an executable file")
	})
}

func TestBridgeName(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		want    string
		wantErr string
	}{
		{"a full id is truncated to twelve", "0123456789abcdefghij", "br-0123456789ab", ""},
		{"surrounding whitespace is trimmed", "\n0123456789abcdef\n", "br-0123456789ab", ""},
		{"exactly twelve", "0123456789ab", "br-0123456789ab", ""},
		{"too short to name a bridge", "0123", "", "too short"},
		{"empty", "", "", "too short"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bridgeName(tt.id)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func slicesIndex(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}
