// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrapdata

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

// validInput is the shape every success case starts from: a TPM-attested
// container node, which is what this provider bootstraps today.
func validInput() Input {
	return Input{
		ClusterName:        "workers",
		MachineName:        "workers-md-0-abcde",
		AttestationMethod:  AttestationMethodTPM,
		SpireServerAddress: "172.26.0.2",
		SpireServerPort:    "30081",
		SpireTrustDomain:   "example.org",
		SpireTrustBundle:   "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		ContainerNode:      true,
	}
}

// parse decodes the rendered document the way a NICo container node does: strip
// the header, then decode what is left. It fails the test on anything the host
// would also reject.
func parse(t *testing.T, document string) map[string]any {
	t.Helper()

	require.True(t, strings.HasPrefix(document, Header), "the document must start with the only header the host strips")

	var decoded map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(strings.TrimPrefix(document, Header)), &decoded),
		"a document that does not parse aborts the instance create and deletes the host")
	require.NotEmpty(t, decoded, "the document must be a non-empty mapping")
	return decoded
}

// runCommands asserts runcmd is a list of plain strings. The host decodes it
// into []string, so cloud-init's list-of-lists form would fail to decode there.
func runCommands(t *testing.T, document map[string]any) []string {
	t.Helper()

	raw, ok := document["runcmd"]
	require.True(t, ok, "runcmd is missing")

	entries, ok := raw.([]any)
	require.True(t, ok, "runcmd must be a list, got %T", raw)
	require.NotEmpty(t, entries)

	commands := make([]string, 0, len(entries))
	for i, entry := range entries {
		command, ok := entry.(string)
		require.True(t, ok, "runcmd[%d] must be a plain string, got %T", i, entry)
		require.NotEmpty(t, strings.TrimSpace(command), "runcmd[%d] is blank", i)
		commands = append(commands, command)
	}
	return commands
}

// writtenFiles maps path to content, asserting the write_files shape the host
// decodes along the way.
func writtenFiles(t *testing.T, document map[string]any) map[string]string {
	t.Helper()

	raw, ok := document["write_files"]
	require.True(t, ok, "write_files is missing")

	entries, ok := raw.([]any)
	require.True(t, ok, "write_files must be a list, got %T", raw)

	files := map[string]string{}
	for i, entry := range entries {
		file, ok := entry.(map[string]any)
		require.True(t, ok, "write_files[%d] must be a mapping, got %T", i, entry)

		path, ok := file["path"].(string)
		require.True(t, ok, "write_files[%d].path must be a string", i)

		content, ok := file["content"].(string)
		require.True(t, ok, "write_files[%d].content must be a string", i)

		permissions, ok := file["permissions"].(string)
		require.True(t, ok, "write_files[%d].permissions must be a string, not an octal number", i)
		assert.Equal(t, "0644", permissions)

		assert.Equal(t, "root:root", file["owner"])

		files[path] = content
	}
	return files
}

func TestRender(t *testing.T) {
	tests := []struct {
		name    string
		input   func() Input
		assert  func(t *testing.T, document string, files map[string]string, commands []string)
		wantErr string
	}{
		{
			name:  "tpm attested container node",
			input: validInput,
			assert: func(t *testing.T, _ string, files map[string]string, commands []string) {
				assert.Contains(t, files, MarkerPath)
				assert.Contains(t, files, SpireAgentConfigPath)
				assert.Contains(t, files, SpireTrustBundlePath)
				assert.Contains(t, files, NodeAgentEnvFilePath)

				config := files[SpireAgentConfigPath]
				assert.Contains(t, config, `server_address = "172.26.0.2"`)
				assert.Contains(t, config, `server_port = "30081"`, "server_port is a quoted string in the known-good configuration")
				assert.Contains(t, config, `trust_domain = "example.org"`)
				assert.Contains(t, config, `trust_bundle_path = "`+SpireTrustBundlePath+`"`)
				assert.Contains(t, config, `NodeAttestor "tpm"`)
				assert.Contains(t, config, `plugin_checksum = "${TPM_PLUGIN_CHECKSUM}"`,
					"the checksum is per build and is expanded on the host, not here")

				assert.Contains(t, files[SpireTrustBundlePath], "BEGIN CERTIFICATE")

				env := files[NodeAgentEnvFilePath]
				assert.Contains(t, env, `AGENT_ARGS="--secure-server-port 15005 --disable-mlx5-rescan"`)
				assert.Contains(t, env, "NKE_LOCAL_DEV=true",
					"replacing this file without re-emitting NKE_LOCAL_DEV breaks kubeadm on a container node")
				assert.NotContains(t, env, "--disable-mtls")

				assert.Contains(t, commands, "systemctl restart "+spireAgentUnit,
					"the unit is enabled with no configuration at boot, so it needs an explicit restart")

				var waits []string
				for _, command := range commands {
					if strings.Contains(command, spireWorkloadSocket) {
						waits = append(waits, command)
					}
				}
				require.Len(t, waits, 1, "exactly one command waits for the workload API socket")
				assert.True(t, strings.HasSuffix(waits[0], "&"), "the wait must be backgrounded: %q", waits[0])
				assert.Contains(t, waits[0], "nohup")
				assert.Contains(t, waits[0], spireWaitLogPath, "the wait must not hold the exec session's stdio open")
				assert.Contains(t, waits[0], "systemctl restart "+nodeAgentUnit)

				assert.Contains(t, commands, "echo success > "+BootstrapSuccessPath)
			},
		},
		{
			name: "attestation method defaults to tpm",
			input: func() Input {
				in := validInput()
				in.AttestationMethod = ""
				return in
			},
			assert: func(t *testing.T, _ string, files map[string]string, _ []string) {
				assert.Contains(t, files[SpireAgentConfigPath], `NodeAttestor "tpm"`)
				assert.Contains(t, files[MarkerPath], "attestation=tpm")
			},
		},
		{
			name: "mTLS disabled leaves SPIRE out entirely",
			input: func() Input {
				in := validInput()
				in.DisableMTLS = true
				in.SpireServerAddress = ""
				in.SpireServerPort = ""
				in.SpireTrustDomain = ""
				in.SpireTrustBundle = ""
				return in
			},
			assert: func(t *testing.T, _ string, files map[string]string, commands []string) {
				assert.NotContains(t, files, SpireAgentConfigPath)
				assert.NotContains(t, files, SpireTrustBundlePath)
				assert.Contains(t, files[NodeAgentEnvFilePath], "--disable-mtls")
				assert.Contains(t, commands, "systemctl restart "+nodeAgentUnit)

				for _, command := range commands {
					assert.NotContains(t, command, spireAgentUnit, "nothing should touch SPIRE: %q", command)
					assert.NotContains(t, command, spireWorkloadSocket, "nothing should wait on SPIRE: %q", command)
				}
			},
		},
		{
			name: "a machine host does not get the container settings",
			input: func() Input {
				in := validInput()
				in.ContainerNode = false
				return in
			},
			assert: func(t *testing.T, _ string, files map[string]string, _ []string) {
				env := files[NodeAgentEnvFilePath]
				assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005\"\n", env)
			},
		},
		{
			name: "a custom port and extra arguments reach the environment file",
			input: func() Input {
				in := validInput()
				in.NodeAgentPort = "15006"
				in.NodeAgentExtraArgs = []string{"--grpc-auth-mode=spiffe-mtls"}
				return in
			},
			assert: func(t *testing.T, _ string, files map[string]string, _ []string) {
				assert.Contains(t, files[NodeAgentEnvFilePath],
					`AGENT_ARGS="--secure-server-port 15006 --disable-mlx5-rescan --grpc-auth-mode=spiffe-mtls"`)
			},
		},
		{
			name: "a trust bundle without a trailing newline still renders",
			input: func() Input {
				in := validInput()
				in.SpireTrustBundle = strings.TrimSuffix(in.SpireTrustBundle, "\n")
				return in
			},
			assert: func(t *testing.T, _ string, files map[string]string, _ []string) {
				assert.True(t, strings.HasSuffix(files[SpireTrustBundlePath], "\n"))
			},
		},
		{
			name: "a name that looks like YAML does not change the document's shape",
			input: func() Input {
				in := validInput()
				in.MachineName = "runcmd: [reboot]\n- path: /etc/passwd"
				return in
			},
			assert: func(t *testing.T, _ string, files map[string]string, commands []string) {
				assert.Len(t, files, 4, "the machine name must not add a file")
				for _, command := range commands {
					assert.NotContains(t, command, "reboot")
				}
			},
		},
		{
			name:    "no cluster name",
			input:   func() Input { in := validInput(); in.ClusterName = ""; return in },
			wantErr: "cluster name is required",
		},
		{
			name:    "no machine name",
			input:   func() Input { in := validInput(); in.MachineName = ""; return in },
			wantErr: "machine name is required",
		},
		{
			name:    "no SPIRE server address",
			input:   func() Input { in := validInput(); in.SpireServerAddress = ""; return in },
			wantErr: "SPIRE server address is required",
		},
		{
			name:    "no trust bundle",
			input:   func() Input { in := validInput(); in.SpireTrustBundle = "   "; return in },
			wantErr: "SPIRE trust bundle is required",
		},
		{
			name:    "a trust domain that would break the HCL quoting",
			input:   func() Input { in := validInput(); in.SpireTrustDomain = `example".org`; return in },
			wantErr: "SPIRE trust domain contains a quote",
		},
		{
			name:    "an attestation method this renderer cannot configure",
			input:   func() Input { in := validInput(); in.AttestationMethod = "joinToken"; return in },
			wantErr: `attestation method "joinToken" is not supported yet`,
		},
		{
			name:    "an agent argument that would word-split",
			input:   func() Input { in := validInput(); in.NodeAgentExtraArgs = []string{"--x a b"}; return in },
			wantErr: "must be a single shell word",
		},
		{
			name:    "an agent argument that would reach the shell",
			input:   func() Input { in := validInput(); in.NodeAgentExtraArgs = []string{"--x=$(id)"}; return in },
			wantErr: "must be a single shell word",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := Render(test.input())

			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				assert.Empty(t, document)
				return
			}

			require.NoError(t, err)

			decoded := parse(t, document)
			commands := runCommands(t, decoded)
			files := writtenFiles(t, decoded)

			assert.Subset(t, []string{"write_files", "runcmd"}, keysOf(decoded),
				"the host honours only write_files and runcmd; anything else is dropped silently")

			if test.assert != nil {
				test.assert(t, document, files, commands)
			}
		})
	}
}

// TestRenderDecodesTheWayTheHostDoes decodes into the same shape the NICo
// container node uses. A document that decodes into a map but not into this
// aborts the instance create, which deletes the host.
func TestRenderDecodesTheWayTheHostDoes(t *testing.T) {
	document, err := Render(validInput())
	require.NoError(t, err)

	var host struct {
		WriteFiles []struct {
			Path        string `yaml:"path"`
			Owner       string `yaml:"owner"`
			Permissions string `yaml:"permissions"`
			Encoding    string `yaml:"encoding"`
			Content     string `yaml:"content"`
		} `yaml:"write_files"`
		RunCmd []string `yaml:"runcmd"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(strings.TrimPrefix(document, Header)), &host))

	require.Len(t, host.WriteFiles, 4)
	require.NotEmpty(t, host.RunCmd)
	for _, file := range host.WriteFiles {
		assert.NotEmpty(t, file.Path)
		assert.NotEmpty(t, file.Content)
		assert.Empty(t, file.Encoding, "plain content needs no encoding, and an unknown one is an error on the host")
	}
}

// TestRenderIsDeterministic keeps the document stable across reconciles. The
// secret is written once, but a renderer that reorders its output makes every
// diff against it meaningless.
func TestRenderIsDeterministic(t *testing.T) {
	first, err := Render(validInput())
	require.NoError(t, err)

	for range 5 {
		again, err := Render(validInput())
		require.NoError(t, err)
		assert.Equal(t, first, again)
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
