// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package join

import (
	"context"
	"fmt"
	"net"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// TransportMode selects how a connection to nke-agent is authenticated.
type TransportMode string

const (
	// TransportSPIFFE is the agent's default: mutual TLS with an X509-SVID
	// from the local SPIRE agent's workload API, authorising exactly the
	// node's expected SPIFFE ID.
	TransportSPIFFE TransportMode = "spiffe"

	// TransportPlaintext matches the agent's --disable-mtls flag, which serves
	// gRPC in the clear on the same port. Development only: anything that can
	// reach the port can then drive kubeadm on that node.
	TransportPlaintext TransportMode = "plaintext"
)

const (
	// DefaultPort is nvke-agent's --secure-server-port default. It has to
	// agree with the port the boot configuration wrote into the node's
	// environment file; see bootstrapdata.DefaultNodeAgentPort.
	DefaultPort = "15005"

	// DefaultWorkloadAPIAddr is the SPIRE agent socket this process reads its
	// own identity from. It is the controller's socket, not the node's.
	DefaultWorkloadAPIAddr = "unix:///tmp/spire-agent/public/api.sock"
)

// DialConfig describes one node's agent endpoint and how to authenticate to it.
type DialConfig struct {
	// Address is the node's host name or IP.
	Address string

	// Port defaults to DefaultPort.
	Port string

	// Mode defaults to TransportSPIFFE. Defaulting to the unauthenticated mode
	// would turn a missing setting into a silent downgrade.
	Mode TransportMode

	// WorkloadAPIAddr is this process's SPIRE agent socket, used in
	// TransportSPIFFE. Empty means DefaultWorkloadAPIAddr.
	WorkloadAPIAddr string

	// ExpectedSPIFFEID is the node agent's own SPIFFE ID, which its
	// certificate must present. Required in TransportSPIFFE: authorising any
	// ID would accept any node in the trust domain.
	ExpectedSPIFFEID string

	// Credentials overrides the mode entirely. It exists for callers that
	// already hold transport credentials, such as a test against a local
	// server.
	Credentials credentials.TransportCredentials
}

// AgentClient is the gRPC-backed Client.
type AgentClient struct {
	conn    *grpc.ClientConn
	agent   nvkeproto.NodeAgentClient
	closers []func() error
}

var _ Client = (*AgentClient)(nil)

// Dial opens a connection to one node's nke-agent.
//
// The connection is lazy — gRPC connects on the first call — so a node whose
// agent is not listening yet fails at ExecuteKubeadmJoin rather than here.
// When it does, the first thing to check is the node's SPIRE registration
// entry: with no entry the agent waits forever for its first SVID and never
// starts listening, with no error and no crash on the node.
//
// The caller closes the result.
func Dial(ctx context.Context, cfg DialConfig) (*AgentClient, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("the node address is required")
	}
	port := cfg.Port
	if port == "" {
		port = DefaultPort
	}

	transportCredentials, closers, err := cfg.transportCredentials(ctx)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(net.JoinHostPort(cfg.Address, port), grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		closeAll(closers)
		return nil, fmt.Errorf("connecting to the node agent at %s: %w", cfg.Address, err)
	}

	return &AgentClient{
		conn:    conn,
		agent:   nvkeproto.NewNodeAgentClient(conn),
		closers: append(closers, conn.Close),
	}, nil
}

// transportCredentials builds the credentials for the configured mode and the
// cleanup the caller owns. The SPIFFE sources hold an open stream to the
// workload API, so they leak until they are closed.
func (c DialConfig) transportCredentials(ctx context.Context) (credentials.TransportCredentials, []func() error, error) {
	if c.Credentials != nil {
		return c.Credentials, nil, nil
	}

	switch c.Mode {
	case TransportPlaintext:
		return insecure.NewCredentials(), nil, nil

	case TransportSPIFFE, "":
		return c.spiffeCredentials(ctx)

	default:
		return nil, nil, fmt.Errorf("unknown transport mode %q", c.Mode)
	}
}

func (c DialConfig) spiffeCredentials(ctx context.Context) (credentials.TransportCredentials, []func() error, error) {
	if c.ExpectedSPIFFEID == "" {
		return nil, nil, fmt.Errorf("the expected SPIFFE ID is required for %s transport", TransportSPIFFE)
	}
	expectedID, err := spiffeid.FromString(c.ExpectedSPIFFEID)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing the expected SPIFFE ID %q: %w", c.ExpectedSPIFFEID, err)
	}

	addr := c.WorkloadAPIAddr
	if addr == "" {
		addr = DefaultWorkloadAPIAddr
	}
	workloadClient, err := workloadapi.New(ctx, workloadapi.WithAddr(addr))
	if err != nil {
		return nil, nil, fmt.Errorf("opening the SPIFFE workload API at %s: %w", addr, err)
	}

	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		_ = workloadClient.Close()
		return nil, nil, fmt.Errorf("creating the X509 source: %w", err)
	}

	closers := []func() error{source.Close, workloadClient.Close}
	return grpccredentials.MTLSClientCredentials(source, source, tlsconfig.AuthorizeID(expectedID)), closers, nil
}

// Close releases the connection and the SPIFFE sources behind it.
func (c *AgentClient) Close() error {
	var err error
	for _, closer := range c.closers {
		if closeErr := closer(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

// ExecuteKubeadmJoin starts the join on the node.
//
// The response status is deliberately not turned into an error. A join
// reissued against an agent that is already joining answers with an error
// status, no gRPC error and no second kubeadm run; treating that as a failure
// would make every retry fail spuriously. Whether the join succeeded is the
// poll loop's answer, not this call's.
func (c *AgentClient) ExecuteKubeadmJoin(ctx context.Context, req Request) error {
	files := make([]*nvkeproto.File, 0, len(req.Files))
	for _, file := range req.Files {
		files = append(files, &nvkeproto.File{
			Path:    file.Path,
			Content: file.Content,
			Mode:    file.Mode,
			Owner:   file.Owner,
		})
	}

	// The command string carries no argument the configuration does not, but
	// the agent reads the node's role out of it, and it is the shape every
	// working caller sends.
	_, err := c.agent.ExecuteKubeadmJoin(ctx, &nvkeproto.KubeadmJoinRequest{
		KubeadmJoinString: req.Material.Command(),
		JoinConfig:        req.Configuration,
		Files:             files,
	})
	if err != nil {
		return fmt.Errorf("calling ExecuteKubeadmJoin: %w", err)
	}
	return nil
}

// KubeadmStatus reads the agent's view of its last kubeadm operation.
func (c *AgentClient) KubeadmStatus(ctx context.Context) (Status, error) {
	resp, err := c.agent.GetKubeadmStatus(ctx, &nvkeproto.KubeadmStatusRequest{})
	if err != nil {
		return Status{}, fmt.Errorf("calling GetKubeadmStatus: %w", err)
	}
	return Status{
		State:        resp.GetKubeadmStatus(),
		Phase:        resp.GetKubeadmPhase(),
		Message:      resp.GetKubeadmCurrentMessage(),
		AgentVersion: resp.GetAgentVersion(),
	}, nil
}

func closeAll(closers []func() error) {
	for _, closer := range closers {
		_ = closer()
	}
}
