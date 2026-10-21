// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"text/template"
	"time"
)

// NicoSiteSpec is the site spec the nico lane bootstraps from: CAPNICo
// allocates the control-plane machines from a NICo site instead of taking them
// from a list of hosts that already exist.
//
// It is a struct rather than a here-document so the fields have names, the
// lane cannot leave one out silently, and a unit test can render it and hand
// the result to the real site spec loader. That test is what catches a schema
// change in the merge gate instead of twenty minutes into a lane.
type NicoSiteSpec struct {
	// ClusterName is the site cluster to build.
	ClusterName string

	// OrgID is the NICo organisation the machines are allocated in.
	OrgID string
	// Endpoint is the NICo API, nico-sim's address in this lane.
	Endpoint string
	// SiteID and VPCID identify the NICo site and its VPC.
	SiteID string
	VPCID  string
	// SubnetIDs, SSHKeyGroupIDs and InstanceTypePreference are what CAPNICo
	// asks NICo for. Each is required; the loader rejects an empty list.
	SubnetIDs              []string
	SSHKeyGroupIDs         []string
	InstanceTypePreference []string

	// NodeCount is how many control-plane machines to allocate.
	NodeCount int
	// Release is the ClusterRelease the nodes boot from, which ReleasesDir
	// must hold.
	Release string
	// ConfigProfile names a config profile. The bootstrap controller serves
	// none unless NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE supplies them.
	ConfigProfile string
	// ReleasesDir holds the MachineImage and ClusterRelease manifests.
	ReleasesDir string

	// ControlPlaneVIP is the API endpoint kube-vip advertises. Empty is
	// allowed only for a one-node control plane.
	ControlPlaneVIP string

	// K8sVersion, CNIName, CNIVersion, PodCIDR and ServiceCIDR describe the
	// cluster to build.
	K8sVersion  string
	CNIName     string
	CNIVersion  string
	PodCIDR     string
	ServiceCIDR string
	// SystemReservedMemory is the kubelet reservation.
	SystemReservedMemory string

	// TrustDomain is the SPIFFE trust domain.
	TrustDomain string
	// SpireExternalAddress is where nodes reach the bootstrap SPIRE server:
	// the host, as the nodes see it. Required unless mTLS is off, because the
	// controller writes it into each node's userdata.
	SpireExternalAddress string
}

// nicoSpecTemplate renders the spec. Field order follows cli/docs/sitespec.md
// so a rendered file and the documentation read the same way.
var nicoSpecTemplate = template.Must(template.New("nico-sitespec").Parse(
	`clusterName: {{ .ClusterName }}
infra:
  provider: nico
  nico:
    orgID: {{ .OrgID }}
    remote:
      endpoint: {{ .Endpoint }}
      siteID: {{ .SiteID }}
      vpcID: {{ .VPCID }}
    controlPlane:
      count: {{ .NodeCount }}
      instanceTypePreference:
{{- range .InstanceTypePreference }}
        - {{ . }}
{{- end }}
      subnetIDs:
{{- range .SubnetIDs }}
        - {{ . }}
{{- end }}
      sshKeyGroupIDs:
{{- range .SSHKeyGroupIDs }}
        - {{ . }}
{{- end }}
      release: {{ .Release }}
{{- if .ConfigProfile }}
      configProfile: {{ .ConfigProfile }}
{{- end }}
    releases: {{ .ReleasesDir }}
cluster:
{{- if .ControlPlaneVIP }}
  controlPlaneVIP: {{ .ControlPlaneVIP }}
{{- end }}
  k8sVersion: "{{ .K8sVersion }}"
  cniName: {{ .CNIName }}
  cniVersion: "{{ .CNIVersion }}"
  podCidr: "{{ .PodCIDR }}"
  serviceCidr: "{{ .ServiceCIDR }}"
  kubeletConfig:
    systemReservedMemory: "{{ .SystemReservedMemory }}"
spire:
  trustDomain: {{ .TrustDomain }}
  externalAddress: {{ .SpireExternalAddress }}
`,
))

// Render returns the site spec as YAML.
func (s NicoSiteSpec) Render() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := nicoSpecTemplate.Execute(&buf, s); err != nil {
		return "", fmt.Errorf("rendering the nico site spec: %w", err)
	}
	return buf.String(), nil
}

// WriteTo renders the spec to path.
func (s NicoSiteSpec) WriteTo(path string) error {
	rendered, err := s.Render()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		return fmt.Errorf("writing the nico site spec to %s: %w", path, err)
	}
	return nil
}

// validate reports the fields a usable spec cannot go without. The loader
// checks them too; checking here names the field the lane failed to set,
// rather than reporting it as a malformed file.
func (s NicoSiteSpec) validate() error {
	missing := []string{}
	for name, value := range map[string]string{
		"ClusterName":          s.ClusterName,
		"OrgID":                s.OrgID,
		"Endpoint":             s.Endpoint,
		"SiteID":               s.SiteID,
		"VPCID":                s.VPCID,
		"Release":              s.Release,
		"ReleasesDir":          s.ReleasesDir,
		"K8sVersion":           s.K8sVersion,
		"TrustDomain":          s.TrustDomain,
		"SpireExternalAddress": s.SpireExternalAddress,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	for name, list := range map[string][]string{
		"SubnetIDs":              s.SubnetIDs,
		"SSHKeyGroupIDs":         s.SSHKeyGroupIDs,
		"InstanceTypePreference": s.InstanceTypePreference,
	} {
		if len(list) == 0 {
			missing = append(missing, name)
		}
	}
	if s.NodeCount < 1 {
		missing = append(missing, "NodeCount")
	}
	if len(missing) > 0 {
		sortStrings(missing)
		return fmt.Errorf("the nico site spec is missing %s", strings.Join(missing, ", "))
	}
	if s.ControlPlaneVIP == "" && s.NodeCount > 1 {
		return fmt.Errorf("a %d-node nico control plane needs a ControlPlaneVIP", s.NodeCount)
	}
	return nil
}

// GatewayOf returns the host's IPv4 address on a nerdctl network: the address
// the containers on it use as their default gateway, and so where nico-sim
// listens and where the nodes reach SPIRE.
func GatewayOf(ctx context.Context, network string) (string, error) {
	out, err := run(ctx, ContainerCLI, "network", "inspect", network,
		"--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return "", fmt.Errorf("reading the gateway of network %q: %w", network, err)
	}
	gateway, err := firstIPv4(out)
	if err != nil {
		return "", fmt.Errorf("network %q: %w", network, err)
	}
	return gateway, nil
}

// firstIPv4 picks the first IPv4 address out of whitespace-separated text,
// skipping the IPv6 entries a dual-stack network reports, which may have no
// gateway at all.
func firstIPv4(s string) (string, error) {
	for _, field := range strings.Fields(s) {
		addr := net.ParseIP(field)
		if addr != nil && addr.To4() != nil {
			return field, nil
		}
	}
	return "", fmt.Errorf("no IPv4 gateway in %q", strings.TrimSpace(s))
}

// WaitForTCP waits until something accepts a connection at address.
func WaitForTCP(ctx context.Context, address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 2*time.Second)
		if err == nil {
			return conn.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", address, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("nothing answered at %s within %s: %w", address, timeout, lastErr)
}

// sortStrings sorts in place; a tiny helper so the error message is stable.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
