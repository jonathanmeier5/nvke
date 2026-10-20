// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package join drives one worker through kubeadm join over nke-agent's gRPC
// API.
//
// Cluster API's bootstrap contract ends at the data secret, and a host that
// joins from cloud-init alone never passes through nke-agent, so its labels,
// taints and node conditions are never set. NVKE's model is imperative: the
// agent is the only sanctioned way a node is changed. The join is therefore an
// RPC, issued once the host exists and the agent answers.
//
// This is the worker path only. Nothing here validates an etcd quota, carries
// a certificate key or an API encryption key, or serialises against another
// join — all of that is control-plane work.
//
// The join material is an input. It is minted against the workload cluster's
// API with the <cluster>-kubeconfig secret, the way CABPK does it, rather than
// read back from a joined control-plane node's agent.
package join

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// The state and phase strings nke-agent reports. They are lowercase constants
// in the agent (agent/internal/agent/agent.go), but every comparison here
// folds case because the NVKE controller's own poll loop does.
const (
	StatePending = "pending"
	StateReady   = "ready"
	StateError   = "error"

	PhaseJoin = "join"
)

const (
	// DefaultPollInterval is the interval the NVKE controller polls the agent
	// at. The agent does no work per poll beyond copying its status struct.
	DefaultPollInterval = 10 * time.Second

	// DefaultTimeout bounds one Join call. The NVKE controller's equivalent is
	// a five-minute Temporal activity whose expiry simply re-fires the join. A
	// reconciler behaves the same way: the call gives up, the next reconcile
	// re-enters the join that is still running rather than starting a second.
	DefaultTimeout = 5 * time.Minute

	// maxFileBytes is the agent's per-file limit. Exceeding it fails the join
	// on the node, after kubeadm has already been committed to.
	maxFileBytes = 1 << 20
)

// ErrTimeout reports that the join did not reach a terminal state inside the
// Joiner's timeout. It is not a failure: the agent runs kubeadm in a goroutine
// and keeps going, so the caller should requeue and poll again rather than
// reset the node.
var ErrTimeout = errors.New("timed out waiting for kubeadm join")

// FailedError reports that kubeadm join failed on the node. Message is the
// agent's last message, which carries kubeadm's own error.
type FailedError struct {
	Message string
}

// Error implements error.
func (e *FailedError) Error() string {
	if e.Message == "" {
		return "kubeadm join failed"
	}
	return "kubeadm join failed: " + e.Message
}

// Client is the slice of nke-agent's gRPC surface this package needs. Dial
// returns the real implementation; tests substitute their own, so nothing
// above this interface speaks gRPC.
type Client interface {
	// ExecuteKubeadmJoin starts a join. It returns once the agent has accepted
	// the request, not once kubeadm has finished: the agent runs kubeadm in a
	// goroutine and reports progress through KubeadmStatus.
	ExecuteKubeadmJoin(ctx context.Context, req Request) error

	// KubeadmStatus returns the agent's view of its last kubeadm operation.
	KubeadmStatus(ctx context.Context) (Status, error)
}

// Material is the join material for one worker, minted against the workload
// cluster's API rather than fetched from a control-plane node.
type Material struct {
	// APIServerEndpoint is the control-plane endpoint in host:port form.
	APIServerEndpoint string

	// Token is a bootstrap token in the id.secret form kubeadm issues.
	Token string

	// CACertHashes are the discovery hashes, each in kubeadm's type:value
	// form, for example sha256:<hex>.
	CACertHashes []string
}

// Command renders the `kubeadm join` line the agent expects.
//
// The literal `kubeadm join ` prefix is contract: the agent derives the node's
// role by looking for `--control-plane` in this string, and the NVKE
// controller's parser rejects anything without the prefix. The string must
// stay free of newlines, tabs and backslashes, which is why Validate rejects
// whitespace inside any field.
func (m Material) Command() string {
	args := make([]string, 0, 5+2*len(m.CACertHashes))
	args = append(args, "kubeadm", "join", m.APIServerEndpoint, "--token", m.Token)
	for _, hash := range m.CACertHashes {
		args = append(args, "--discovery-token-ca-cert-hash", hash)
	}
	return strings.Join(args, " ")
}

// Validate reports whether the material can be rendered into a usable command.
func (m Material) Validate() error {
	if err := validateField("the API server endpoint", m.APIServerEndpoint); err != nil {
		return err
	}
	if err := validateField("the bootstrap token", m.Token); err != nil {
		return err
	}
	if len(m.CACertHashes) == 0 {
		return errors.New("at least one discovery CA certificate hash is required")
	}
	for _, hash := range m.CACertHashes {
		if err := validateField("the discovery CA certificate hash", hash); err != nil {
			return err
		}
		// kubeadm takes type:value, and a bare hex digest is the easy mistake.
		// It is rejected on the node, long after the join has started.
		if !strings.Contains(hash, ":") {
			return fmt.Errorf("the discovery CA certificate hash %q must use kubeadm's type:value form, for example sha256:<hex>", hash)
		}
	}
	return nil
}

// validateField rejects empty values and any whitespace inside one. The agent
// splits the rendered command on whitespace, so an embedded space silently
// becomes two argv entries.
func validateField(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		return fmt.Errorf("%s must not contain whitespace: %q", name, value)
	}
	return nil
}

// File is one file staged on the node before kubeadm runs. A worker needs the
// kubelet patch the join configuration's patches directory points at.
type File struct {
	// Path is absolute. The agent creates the parent directory.
	Path string

	// Content is at most 1 MiB.
	Content []byte

	// Mode holds permission bits only, and must be set: the agent rejects a
	// zero mode rather than writing an unreadable file.
	Mode uint32

	// Owner is in user:group form, for example root:root.
	Owner string
}

// Request is one worker's join.
type Request struct {
	// Material is the discovery material the rendered command carries.
	Material Material

	// Configuration is the rendered kubeadm JoinConfiguration document.
	//
	// It is required, not optional. The agent only runs the command string
	// when this is empty, and that path is broken: it passes the whole
	// `kubeadm join …` string as argv to the `kubeadm` binary, so the node
	// runs `kubeadm kubeadm join …`
	// (agent/internal/agent/agent.go, processKubeadmJoin). Every working
	// caller sends a configuration, and a container-backed worker needs one
	// anyway for nodeRegistration.ignorePreflightErrors: [SystemVerification].
	Configuration string

	// Files are staged before kubeadm runs.
	Files []File
}

// Validate reports whether the agent would accept the request. The agent's own
// checks run after it has returned OK, so a rejection there surfaces as a
// failed join several seconds later instead of as an error here.
func (r Request) Validate() error {
	if err := r.Material.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Configuration) == "" {
		return errors.New("the kubeadm join configuration is required")
	}
	// The agent reads the node's role out of the documents it is given. A
	// control-plane marker here would make it join as a control plane with
	// none of the control-plane material this package carries.
	if strings.Contains(r.Configuration, "controlPlane:") {
		return errors.New("the join configuration declares a control plane; this package joins workers only")
	}
	seen := make(map[string]struct{}, len(r.Files))
	for i, file := range r.Files {
		if err := file.validate(seen); err != nil {
			return fmt.Errorf("file %d: %w", i, err)
		}
	}
	return nil
}

// validate mirrors the agent's own file checks so a bad file is caught before
// the join starts rather than after.
func (f File) validate(seen map[string]struct{}) error {
	if f.Path == "" {
		return errors.New("path is required")
	}
	cleaned := filepath.Clean(f.Path)
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("path must be absolute: %q", f.Path)
	}
	if _, duplicate := seen[cleaned]; duplicate {
		return fmt.Errorf("duplicate path %s", cleaned)
	}
	seen[cleaned] = struct{}{}

	if len(f.Content) > maxFileBytes {
		return fmt.Errorf("content exceeds the agent's %d byte limit: %d bytes", maxFileBytes, len(f.Content))
	}
	if f.Mode == 0 {
		return fmt.Errorf("mode is required for %s", cleaned)
	}
	if f.Mode&^uint32(0o777) != 0 {
		return fmt.Errorf("mode for %s must contain only permission bits: %#o", cleaned, f.Mode)
	}
	if strings.Count(f.Owner, ":") != 1 {
		return fmt.Errorf("owner for %s must use user:group form, got %q", cleaned, f.Owner)
	}
	return nil
}

// Status is nke-agent's view of its last kubeadm operation.
type Status struct {
	// State is the agent's kubeadm_status: pending, ready or error.
	State string

	// Phase is the agent's kubeadm_phase: join or initialize.
	Phase string

	// Message is the agent's last message. On a failure it carries kubeadm's
	// own error.
	Message string

	// AgentVersion is reported for logging. It also decides which unit name
	// and environment file path the node answers to, which matters to callers
	// that touch either.
	AgentVersion string
}

// Joined reports a finished, successful join.
func (s Status) Joined() bool {
	return equalFold(s.State, StateReady) && equalFold(s.Phase, PhaseJoin)
}

// Joining reports a join the agent has accepted and not yet finished.
func (s Status) Joining() bool {
	return equalFold(s.State, StatePending) && equalFold(s.Phase, PhaseJoin)
}

// Failed reports that the last kubeadm operation ended in an error.
//
// The phase is deliberately not part of the test, matching the NVKE
// controller's poll loop: the agent sets the error state before it sets the
// phase on some paths, and a worker never runs any phase but join.
func (s Status) Failed() bool {
	return equalFold(s.State, StateError)
}

// String renders the status for a log line or a timeout message.
func (s Status) String() string {
	return fmt.Sprintf("state=%q phase=%q message=%q", s.State, s.Phase, s.Message)
}

func equalFold(value, want string) bool {
	return strings.EqualFold(strings.TrimSpace(value), want)
}

// Outcome says what a Join call did. A resumed join is not a fresh one, and a
// caller writing conditions or events needs to tell them apart.
type Outcome string

const (
	// OutcomeJoined means this call issued the join and drove it to done.
	OutcomeJoined Outcome = "Joined"

	// OutcomeAlreadyJoined means the agent had already finished a join, so no
	// RPC was issued. This is the idempotent path a retry takes.
	OutcomeAlreadyJoined Outcome = "AlreadyJoined"

	// OutcomeResumed means a join was already in flight, so this call polled
	// it instead of starting a second one.
	OutcomeResumed Outcome = "Resumed"
)

// Result is what one Join call did and the status it ended on.
type Result struct {
	// Outcome is set only when Join returned no error. An error says what
	// happened more precisely than any outcome could.
	Outcome Outcome

	// Status is the last status read from the agent, set either way.
	Status Status
}

// Joiner drives one worker's join through a Client.
//
// It holds no state. Idempotency comes from the agent's reported status, which
// is a live signal and not a durable one: the agent does not reload its state
// file at startup, so an agent restarted mid-join reports an uninitialized
// status and Join will issue a second kubeadm join over the leftovers. A
// caller that must not repeat a join across an agent restart has to record
// that itself, on the object it reconciles.
type Joiner struct {
	// Client talks to one node's agent.
	Client Client

	// PollInterval defaults to DefaultPollInterval.
	PollInterval time.Duration

	// Timeout bounds Join and Wait, and defaults to DefaultTimeout. The
	// context's own deadline still applies and is reported separately.
	Timeout time.Duration

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Completed reports whether the node has already finished a join.
//
// This is the idempotency check. Call it before anything that would repeat
// work: a true answer means the node is joined and nothing further should be
// issued to it.
func (j *Joiner) Completed(ctx context.Context) (bool, error) {
	status, err := j.Client.KubeadmStatus(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the agent's kubeadm status: %w", err)
	}
	return status.Joined(), nil
}

// Join drives one worker to a joined state.
//
// It is safe to call again. A completed join returns without issuing an RPC; a
// join already in flight is polled rather than restarted; a previously failed
// join is reissued, and the agent runs kubeadm reset before retrying.
//
// A failure returns a *FailedError, a join still running at the timeout
// returns ErrTimeout, and both carry the status the node ended on.
func (j *Joiner) Join(ctx context.Context, req Request) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, fmt.Errorf("validating the join request: %w", err)
	}

	status, err := j.Client.KubeadmStatus(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("reading the agent's kubeadm status: %w", err)
	}

	switch {
	case status.Joined():
		j.logger().Debug("the node has already joined", "status", status.String())
		return Result{Outcome: OutcomeAlreadyJoined, Status: status}, nil

	case status.Joining():
		// Reissuing here is tolerated by the agent, which answers "already
		// joined or pending join" with no gRPC error, but polling the join
		// that is running is the honest thing to do.
		j.logger().Info("a join is already in flight; waiting for it", "status", status.String())
		final, waitErr := j.Wait(ctx)
		if waitErr != nil {
			return Result{Status: final}, waitErr
		}
		return Result{Outcome: OutcomeResumed, Status: final}, nil
	}

	j.logger().Info("issuing kubeadm join",
		"endpoint", req.Material.APIServerEndpoint,
		"files", len(req.Files),
		"agentVersion", status.AgentVersion,
	)
	if err := j.Client.ExecuteKubeadmJoin(ctx, req); err != nil {
		return Result{Status: status}, fmt.Errorf("issuing kubeadm join: %w", err)
	}

	final, err := j.Wait(ctx)
	if err != nil {
		return Result{Status: final}, err
	}
	return Result{Outcome: OutcomeJoined, Status: final}, nil
}

// Wait polls the agent until the join is done, has failed, or the budget runs
// out. It issues no join of its own, so it is the right call for a reconcile
// that is resuming one.
func (j *Joiner) Wait(ctx context.Context) (Status, error) {
	timeout := j.timeout()
	expiry := time.NewTimer(timeout)
	defer expiry.Stop()

	ticker := time.NewTicker(j.pollInterval())
	defer ticker.Stop()

	var status Status
	for {
		var err error
		status, err = j.Client.KubeadmStatus(ctx)
		if err != nil {
			return status, fmt.Errorf("reading the agent's kubeadm status: %w", err)
		}

		switch {
		case status.Failed():
			return status, &FailedError{Message: status.Message}
		case status.Joined():
			j.logger().Info("the node joined", "status", status.String())
			return status, nil
		}

		select {
		case <-ctx.Done():
			return status, fmt.Errorf("waiting for kubeadm join: %w", ctx.Err())
		case <-expiry.C:
			return status, fmt.Errorf("%w after %s, last %s", ErrTimeout, timeout, status.String())
		case <-ticker.C:
		}
	}
}

func (j *Joiner) pollInterval() time.Duration {
	if j.PollInterval > 0 {
		return j.PollInterval
	}
	return DefaultPollInterval
}

func (j *Joiner) timeout() time.Duration {
	if j.Timeout > 0 {
		return j.Timeout
	}
	return DefaultTimeout
}

func (j *Joiner) logger() *slog.Logger {
	if j.Logger != nil {
		return j.Logger
	}
	return slog.Default()
}
