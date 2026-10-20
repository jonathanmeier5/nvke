// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bootstrapdata renders the boot configuration a host receives before
// it is joined.
//
// The payload is cloud-config, because that is what the infrastructure provider
// hands to NICo as user data. It carries only what the host needs to come up
// and become reachable: the SPIRE agent's configuration and trust bundle, the
// node agent's environment file, and the commands that start both. The kubeadm
// join configuration is not in it. The join is driven over gRPC once nke-agent
// answers, so nothing joinable is written to a secret at rest and the join
// material is built after the control-plane endpoint is known.
//
// # What a NICo container node actually honours
//
// A NICo container instance does not run cloud-init. Its user-data handler
// understands write_files and runcmd and silently drops every other directive,
// so nothing else may be relied on here. Two consequences shape this package:
//
//   - runcmd is decoded into a list of plain strings. cloud-init's alternative
//     list-of-lists form fails to decode, and a decode failure aborts the
//     instance create, which rolls the container back. The failure surfaces as
//     a vanished machine, not as a configuration error on the host.
//   - Every runcmd entry runs synchronously inside the instance-create request,
//     under a single budget for all of them. Anything that waits must be
//     backgrounded.
package bootstrapdata

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Header is the only prefix a NICo container node strips. Anything else — a
// bare script, MIME multipart, gzip — is not recognised, and the instance
// create fails rather than ignoring it.
const Header = "#cloud-config\n"

// Paths written on the host. The SPIRE agent's configuration path is fixed by
// its systemd unit's ExecStart, so writing the configuration anywhere else is a
// silent no-op; the trust bundle path is free and only has to agree with
// trust_bundle_path in the configuration.
const (
	// MarkerPath is written on every host this provider bootstraps. It is proof
	// the payload survived the whole path: provider -> secret -> infrastructure
	// provider -> NICo -> host.
	MarkerPath = "/etc/nvke/bootstrap-provider"

	// SpireAgentConfigPath is `spire-agent run -config`'s argument in the unit.
	SpireAgentConfigPath = "/etc/spire-agent/agent.conf"

	// SpireTrustBundlePath matches trust_bundle_path in the rendered config.
	SpireTrustBundlePath = "/etc/spire-agent/initial-trust-bundle.pem"

	// NodeAgentEnvFilePath is the systemd EnvironmentFile nvke-agent.service
	// reads. /etc/default/nke-agent is a symlink to it on current images, so
	// writing both paths would mean writing one file twice.
	NodeAgentEnvFilePath = "/etc/default/nvke-agent"

	// BootstrapSuccessPath is the marker Cluster API's own tooling looks for to
	// decide that bootstrap finished.
	BootstrapSuccessPath = "/run/cluster-api/bootstrap-success.complete"
)

// DefaultNodeAgentPort is nvke-agent's own default for --secure-server-port and
// the port the controller dials. The two have to agree, so it is stated rather
// than left implicit.
const DefaultNodeAgentPort = "15005"

const (
	// nodeAgentUnit is the unit name used by agent v0.3.0 and later.
	// nke-agent.service is an alias of it on current images, but the alias is
	// not guaranteed on older ones, so address the canonical name.
	nodeAgentUnit = "nvke-agent.service"

	spireAgentUnit = "spire-agent.service"

	// spireWorkloadSocket is spire-agent 1.14's compiled-in default workload
	// API socket and nvke-agent's default --spiffe-socket-path. Neither side
	// sets it explicitly, so the defaults have to match — they do.
	spireWorkloadSocket = "/tmp/spire-agent/public/api.sock"

	// spireAgentDataDir persists through the host's /var volume. The e2e
	// substrate instead puts it on tmpfs so state is discarded on every
	// restart; this matches the CAPI path, which is the configuration the node
	// image is known to work with.
	// FIXME: https://github.com/dsx-ai-factory/nvke/issues/<N> — a persisted SVID
	// signed by a CA that no longer exists makes the agent fail rather than
	// re-attest. Revisit once rebootstrap_mode is verified against this image.
	spireAgentDataDir = "/var/lib/spire-agent"

	// spireSocketWaitSeconds bounds the backgrounded wait for the workload API
	// socket. nke-agent blocks on its first SVID, so starting it before the
	// socket exists only produces a process that never listens.
	spireSocketWaitSeconds = 90

	// systemReadyWaitSeconds bounds the wait for systemd to finish booting.
	// Nothing in the instance-create path waits between starting the container
	// and running the first command, so the first systemctl call races D-Bus.
	systemReadyWaitSeconds = 60

	// spireWaitLogPath keeps the backgrounded wait's output off the exec
	// session's stdio. Without the redirect the instance create would block on
	// the open file descriptors it inherited.
	spireWaitLogPath = "/var/log/nvke-agent-spire-wait.log"
)

// Input is what the rendered document describes.
//
// The SPIRE fields are required unless DisableMTLS is set: without them the
// agent has no configuration, its unit restarts every few seconds forever, and
// nke-agent never finishes starting.
type Input struct {
	ClusterName string
	MachineName string
	// AttestationMethod selects the SPIRE node attestor. Empty means tpm.
	AttestationMethod string

	// SpireServerAddress and SpireServerPort are where the agent attests.
	SpireServerAddress string
	SpireServerPort    string

	// SpireTrustDomain is the trust domain of that server.
	SpireTrustDomain string

	// SpireTrustBundle is the server's CA bundle, PEM encoded. The agent needs
	// it to authenticate the server before it holds any identity of its own.
	SpireTrustBundle string

	// NodeAgentPort is nvke-agent's --secure-server-port. Empty means
	// DefaultNodeAgentPort.
	NodeAgentPort string

	// NodeAgentExtraArgs are appended to the agent's flags verbatim. systemd
	// expands $AGENT_ARGS unquoted, so each entry must be a single shell word.
	NodeAgentExtraArgs []string

	// DisableMTLS serves nvke-agent's gRPC in plaintext and leaves SPIRE out of
	// the document entirely. Development only: anything on the node's network
	// can then drive kubeadm on it.
	DisableMTLS bool

	// ContainerNode marks the host as a NICo container rather than a machine.
	// It re-emits the two settings the node image ships in the environment file
	// this document replaces, both of which a container join needs.
	ContainerNode bool
}

// AttestationMethodTPM is the only attestor this renderer emits today.
const AttestationMethodTPM = "tpm"

// cloudConfig is the subset of the cloud-config schema a NICo container node
// decodes. It is deliberately the same shape as that decoder's, so marshalling
// through it cannot produce a document the host would reject.
type cloudConfig struct {
	WriteFiles []writeFile `yaml:"write_files"`
	RunCmd     []string    `yaml:"runcmd"`
}

type writeFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

// Render returns the cloud-config document for one host.
//
// The document is marshalled by a YAML encoder rather than assembled as text,
// because a certificate bundle or a quoted shell command pasted into a
// hand-indented template is one escaping mistake away from a document that does
// not parse — and on a NICo container node that mistake deletes the instance
// instead of reporting itself. The result is parsed back before it is returned,
// with the decoder the host uses, so a document that would not survive the trip
// fails here instead.
func Render(in Input) (string, error) {
	in, err := in.normalize()
	if err != nil {
		return "", err
	}

	cfg := cloudConfig{
		WriteFiles: []writeFile{rootFile(MarkerPath, "0644", in.marker())},
		RunCmd:     []string{waitForSystemd()},
	}

	if !in.DisableMTLS {
		agentConf, err := in.spireAgentConfig()
		if err != nil {
			return "", err
		}
		cfg.WriteFiles = append(cfg.WriteFiles,
			rootFile(SpireAgentConfigPath, "0644", agentConf),
			rootFile(SpireTrustBundlePath, "0644", ensureTrailingNewline(in.SpireTrustBundle)),
		)
	}

	nodeAgentEnv, err := in.nodeAgentEnvFile()
	if err != nil {
		return "", err
	}
	cfg.WriteFiles = append(cfg.WriteFiles, rootFile(NodeAgentEnvFilePath, "0644", nodeAgentEnv))

	if in.DisableMTLS {
		// No SPIRE means no socket to wait for, so the agent can be started
		// straight away rather than from a backgrounded watcher.
		cfg.RunCmd = append(cfg.RunCmd, "systemctl restart "+nodeAgentUnit)
	} else {
		// The unit is enabled in the image but has no configuration until the
		// write_files above land, so it has been failing and restarting on a
		// timer since boot. An explicit restart is what makes it pick the
		// configuration up at a known point rather than whenever its next retry
		// happens to fall.
		cfg.RunCmd = append(cfg.RunCmd,
			"systemctl restart "+spireAgentUnit,
			restartNodeAgentWhenSpireIsReady(),
		)
	}

	cfg.RunCmd = append(cfg.RunCmd, markBootstrapSuccess()...)

	return encode(cfg)
}

// normalize applies defaults and rejects input that would render a document the
// host accepts but cannot act on.
func (in Input) normalize() (Input, error) {
	if in.ClusterName == "" {
		return in, fmt.Errorf("cluster name is required")
	}
	if in.MachineName == "" {
		return in, fmt.Errorf("machine name is required")
	}

	if in.NodeAgentPort == "" {
		in.NodeAgentPort = DefaultNodeAgentPort
	}
	if err := validShellWord("node agent port", in.NodeAgentPort); err != nil {
		return in, err
	}
	for _, arg := range in.NodeAgentExtraArgs {
		if err := validShellWord("node agent argument", arg); err != nil {
			return in, err
		}
	}

	if in.DisableMTLS {
		return in, nil
	}

	if in.AttestationMethod == "" {
		in.AttestationMethod = AttestationMethodTPM
	}
	if in.AttestationMethod != AttestationMethodTPM {
		// joinToken needs a token minted against the SPIRE server's Agent API
		// before the host exists, and dsxImds needs the plugin checksum and
		// endpoint the DSX image supplies. Neither is plumbed into this
		// provider, and rendering an attestor without them produces a host that
		// boots, looks healthy and never attests.
		// FIXME: https://github.com/dsx-ai-factory/nvke/issues/<N> — support
		// joinToken and dsxImds once their inputs reach this provider.
		return in, fmt.Errorf("attestation method %q is not supported yet, only %q is", in.AttestationMethod, AttestationMethodTPM)
	}

	for _, field := range []struct {
		name  string
		value string
	}{
		{"SPIRE server address", in.SpireServerAddress},
		{"SPIRE server port", in.SpireServerPort},
		{"SPIRE trust domain", in.SpireTrustDomain},
	} {
		if field.value == "" {
			return in, fmt.Errorf("%s is required unless mTLS is disabled", field.name)
		}
		// The values are interpolated into double-quoted HCL strings.
		if strings.ContainsAny(field.value, "\"\\\n\r") {
			return in, fmt.Errorf("%s contains a quote, a backslash or a newline: %q", field.name, field.value)
		}
	}

	if strings.TrimSpace(in.SpireTrustBundle) == "" {
		return in, fmt.Errorf("SPIRE trust bundle is required unless mTLS is disabled")
	}

	return in, nil
}

// marker records which cluster and machine this host was bootstrapped for.
func (in Input) marker() string {
	var b strings.Builder
	b.WriteString("# Written by the NVKE bootstrap provider.\n")
	fmt.Fprintf(&b, "cluster=%s\n", in.ClusterName)
	fmt.Fprintf(&b, "machine=%s\n", in.MachineName)
	if in.DisableMTLS {
		b.WriteString("attestation=none\n")
		return b.String()
	}
	fmt.Fprintf(&b, "attestation=%s\n", in.AttestationMethod)
	return b.String()
}

// spireAgentConfig renders the agent's HCL configuration.
//
// It follows the configuration the node image is known to work with, field for
// field, including server_port as a quoted string. ${TPM_PLUGIN_CHECKSUM} is
// left for the agent to expand: the unit runs it with -expandEnv and sources
// the checksum from an environment file the image ships, and the value changes
// with every build of the attestor plugin, so resolving it here would pin the
// document to one image.
func (in Input) spireAgentConfig() (string, error) {
	var b strings.Builder

	b.WriteString("agent {\n")
	fmt.Fprintf(&b, "    data_dir = %q\n", spireAgentDataDir)
	b.WriteString("    log_level = \"DEBUG\"\n\n")
	fmt.Fprintf(&b, "    server_address = %q\n", in.SpireServerAddress)
	fmt.Fprintf(&b, "    server_port = %q\n\n", in.SpireServerPort)
	fmt.Fprintf(&b, "    trust_domain = %q\n", in.SpireTrustDomain)
	fmt.Fprintf(&b, "    trust_bundle_path = %q\n", SpireTrustBundlePath)
	b.WriteString("}\n\n")

	b.WriteString("plugins {\n")
	b.WriteString("    KeyManager \"disk\" {\n")
	b.WriteString("        plugin_data {\n")
	fmt.Fprintf(&b, "            directory = %q\n", spireAgentDataDir)
	b.WriteString("        }\n")
	b.WriteString("    }\n\n")

	switch in.AttestationMethod {
	case AttestationMethodTPM:
		b.WriteString("    NodeAttestor \"tpm\" {\n")
		b.WriteString("        plugin_cmd = \"/usr/local/lib/spire-agent/tpm_attestor_agent\"\n")
		b.WriteString("        plugin_checksum = \"${TPM_PLUGIN_CHECKSUM}\"\n")
		b.WriteString("        plugin_data {}\n")
		b.WriteString("    }\n\n")
	default:
		return "", fmt.Errorf("no node attestor for attestation method %q", in.AttestationMethod)
	}

	b.WriteString("    WorkloadAttestor \"unix\" {\n")
	b.WriteString("        plugin_data {}\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")

	return b.String(), nil
}

// nodeAgentEnvFile renders nvke-agent's systemd EnvironmentFile.
//
// Writing this file replaces the one the node image ships, which also carries
// settings this provider did not put there. The two that matter are re-emitted
// for a container host: --disable-mlx5-rescan, without which the agent spends
// minutes at startup rescanning hardware a container does not have, and
// NKE_LOCAL_DEV, which is the only thing that makes kubeadm skip the system
// verification a container cannot pass. Dropping the second turns every join on
// a container node into a preflight failure.
func (in Input) nodeAgentEnvFile() (string, error) {
	args := []string{"--secure-server-port", in.NodeAgentPort}
	if in.DisableMTLS {
		args = append(args, "--disable-mtls")
	}
	if in.ContainerNode {
		args = append(args, "--disable-mlx5-rescan")
	}
	args = append(args, in.NodeAgentExtraArgs...)

	// systemd does not run a shell, and ExecStart expands $AGENT_ARGS unquoted,
	// so the value is one flat word-split string and quoting inside it does not
	// group anything. normalize has already rejected arguments that would not
	// survive that, so this is the assertion that it did.
	joined := strings.Join(args, " ")
	if err := validEnvValue("AGENT_ARGS", joined); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "AGENT_ARGS=%q\n", joined)
	if in.ContainerNode {
		b.WriteString("NKE_LOCAL_DEV=true\n")
	}
	return b.String(), nil
}

// waitForSystemd keeps the first systemctl call from racing a container that
// has only just been started. The timeout bounds it against the budget the
// whole runcmd list shares, and the failure is swallowed because a degraded
// system is still one that answers systemctl.
func waitForSystemd() string {
	return fmt.Sprintf("timeout %d systemctl is-system-running --wait >/dev/null 2>&1 || true", systemReadyWaitSeconds)
}

// restartNodeAgentWhenSpireIsReady backgrounds the wait for the SPIRE workload
// API socket.
//
// The agent blocks until its first SVID arrives, so restarting it before SPIRE
// has attested leaves a process that is up and never listening. Waiting in the
// foreground instead would spend the instance-create budget on it and roll the
// host back, so the wait is detached and its output redirected: the create only
// has to start this, not see it finish.
func restartNodeAgentWhenSpireIsReady() string {
	script := fmt.Sprintf(
		`n=0; while [ "$n" -lt %d ]; do if [ -S %s ]; then systemctl restart %s; exit 0; fi; n=$((n+1)); sleep 1; done; exit 1`,
		spireSocketWaitSeconds, spireWorkloadSocket, nodeAgentUnit,
	)
	return fmt.Sprintf("nohup sh -c '%s' </dev/null >%s 2>&1 &", script, spireWaitLogPath)
}

// markBootstrapSuccess writes the marker Cluster API treats as "bootstrap ran".
func markBootstrapSuccess() []string {
	dir := BootstrapSuccessPath[:strings.LastIndex(BootstrapSuccessPath, "/")]
	return []string{
		"mkdir -p " + dir,
		"echo success > " + BootstrapSuccessPath,
	}
}

// rootFile is a write_files entry owned by root. Every file this provider
// writes is read by a process running as root before any other user exists on
// the host, so none of them needs a different owner.
func rootFile(path, permissions, content string) writeFile {
	return writeFile{
		Path:        path,
		Owner:       "root:root",
		Permissions: permissions,
		Content:     content,
	}
}

// encode marshals the document and proves it survives the host's decoder.
//
// The round trip is not a formality. The host decodes runcmd into a list of
// plain strings and a document that does not fit that shape aborts the instance
// create, which deletes the container — so the failure arrives as a machine
// that never appeared, with nothing on the host to read. Catching it here turns
// that into an error on the NvkeConfig.
func encode(cfg cloudConfig) (string, error) {
	var buf strings.Builder
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(cfg); err != nil {
		return "", fmt.Errorf("encoding the cloud-config document: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf("closing the cloud-config encoder: %w", err)
	}

	document := Header + buf.String()

	var decoded cloudConfig
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(document, Header)), &decoded); err != nil {
		return "", fmt.Errorf("the rendered cloud-config does not parse: %w", err)
	}
	if len(decoded.WriteFiles) != len(cfg.WriteFiles) || len(decoded.RunCmd) != len(cfg.RunCmd) {
		return "", fmt.Errorf("the rendered cloud-config lost entries: %d/%d files, %d/%d commands",
			len(decoded.WriteFiles), len(cfg.WriteFiles), len(decoded.RunCmd), len(cfg.RunCmd))
	}
	for i, want := range cfg.WriteFiles {
		if decoded.WriteFiles[i] != want {
			return "", fmt.Errorf("the rendered cloud-config changed %s on the round trip", want.Path)
		}
	}
	for i, want := range cfg.RunCmd {
		if decoded.RunCmd[i] != want {
			return "", fmt.Errorf("the rendered cloud-config changed command %d on the round trip", i)
		}
	}

	return document, nil
}

// shellMetacharacters is every byte that stops a value being one shell word.
// The backslash and the backtick are concatenated separately so neither needs
// an escape that is easy to misread.
const shellMetacharacters = " \t\n\r\"'$;&|<>()" + "`" + `\`

// validShellWord rejects a value that would not survive $AGENT_ARGS being
// expanded unquoted by systemd.
func validShellWord(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if strings.ContainsAny(value, shellMetacharacters) {
		return fmt.Errorf("%s %q must be a single shell word: systemd expands $AGENT_ARGS unquoted", name, value)
	}
	return nil
}

// validEnvValue rejects a value that would not survive being read back out of a
// systemd EnvironmentFile.
func validEnvValue(name, value string) error {
	if strings.ContainsAny(value, "\n\r\"\\") {
		return fmt.Errorf("%s contains a quote, a backslash or a newline: %q", name, value)
	}
	return nil
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
