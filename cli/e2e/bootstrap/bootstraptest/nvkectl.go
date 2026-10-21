// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Milestones `bootstrap site` logs on its way through a successful run. The
// lanes key on these rather than on exit status alone: the command can exit 0
// having skipped a stage, and a stage that stops being reported is a
// regression the exit status does not show.
const (
	// MilestoneKindClusterReady is logged once the ephemeral bootstrap Kind
	// cluster is up, before the controller is deployed. It is the point at
	// which a lane may bridge that cluster to its node network.
	MilestoneKindClusterReady = "bootstrap kind cluster ready"
	// MilestoneControllerHealthy is logged when the controller answers /readyz.
	MilestoneControllerHealthy = "controller deployment healthy"
	// MilestoneClusterProvisioned is logged when the controller reports the
	// site cluster provisioned.
	MilestoneClusterProvisioned = "cluster provisioned"
)

// Logf is a logging sink, satisfied by (*testing.T).Logf.
type Logf func(format string, args ...any)

// BootstrapOptions are the inputs to one `nvkectl bootstrap site` run. Only
// Binary and SiteSpec are required; an unset field leaves the flag off, so the
// command's own default applies.
type BootstrapOptions struct {
	// Binary is the nvkectl to run. See FindNvkectl.
	Binary string
	// SiteSpec is the site spec file, the -f argument.
	SiteSpec string

	// KubeconfigOut is where the finished cluster's kubeconfig is written.
	// A lane that asserts anything about the cluster needs it.
	KubeconfigOut string
	// EncryptionKeyOut is where the etcd encryption key is written. The
	// command refuses to overwrite one, so each run needs its own path.
	EncryptionKeyOut string
	// DiagnosticsDir is where a failed bootstrap dumps cluster state.
	DiagnosticsDir string

	// Timeout is the command's own --timeout, bounding how long it waits for
	// the site cluster. It is not a bound on this process: see Bootstrap.Wait.
	Timeout time.Duration

	// KeepBootstrapCluster leaves the ephemeral Kind cluster standing, so the
	// test can read it afterwards. The test then owns deleting it.
	KeepBootstrapCluster bool
	// DisableMTLS stands the site up without SPIRE.
	DisableMTLS bool

	// Env is added to the environment the command inherits, as "K=V" entries.
	Env []string
}

// args renders the options as the command line, in a fixed order so a test can
// state the whole of it.
func (o BootstrapOptions) args() []string {
	args := []string{"bootstrap", "site", "-f", o.SiteSpec}
	if o.KeepBootstrapCluster {
		args = append(args, "--keep-bootstrap-cluster")
	}
	if o.DisableMTLS {
		args = append(args, "--disable-mtls")
	}
	if o.Timeout > 0 {
		args = append(args, "--timeout", o.Timeout.String())
	}
	if o.KubeconfigOut != "" {
		args = append(args, "--kubeconfig-out", o.KubeconfigOut)
	}
	if o.EncryptionKeyOut != "" {
		args = append(args, "--encryption-key-out", o.EncryptionKeyOut)
	}
	if o.DiagnosticsDir != "" {
		args = append(args, "--diagnostics-dir", o.DiagnosticsDir)
	}
	return args
}

// validate reports the options a run cannot start without.
func (o BootstrapOptions) validate() error {
	switch {
	case o.Binary == "":
		return errors.New("bootstraptest: BootstrapOptions.Binary is empty: name the nvkectl to run")
	case o.SiteSpec == "":
		return errors.New("bootstraptest: BootstrapOptions.SiteSpec is empty: name the site spec to bootstrap from")
	}
	return nil
}

// Bootstrap is one running `nvkectl bootstrap site`.
//
// The command runs in a process group of its own. nvkectl does not forward
// signals to the kind, helm and nerdctl processes it drives, so stopping it
// means signalling the group; a test that abandoned the direct child would
// leave those running and the next run would trip over what they hold.
type Bootstrap struct {
	cmd *exec.Cmd
	out *syncBuffer

	// finished is closed once cmd.Wait has returned, having set waitErr.
	// Closing it rather than sending on it is what makes Wait, Stop and
	// WaitForMilestone safe to call in any order and more than once.
	finished chan struct{}
	waitErr  error

	stopOnce sync.Once
}

// StartBootstrap starts `nvkectl bootstrap site` and returns without waiting
// for it. Output is captured and, line by line, handed to logf.
//
// The caller must call Stop, normally deferred: a test that fails part way
// through otherwise leaves a bootstrap running.
func StartBootstrap(ctx context.Context, opts BootstrapOptions, logf Logf) (*Bootstrap, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	args := opts.args()
	cmd := exec.CommandContext(ctx, opts.Binary, args...)
	cmd.Env = append(os.Environ(), opts.Env...)
	out := &syncBuffer{}
	cmd.Stdout = io.MultiWriter(out, newLineWriter(logf))
	cmd.Stderr = cmd.Stdout
	setProcessGroup(cmd)

	logf("bootstraptest: running %s %s", opts.Binary, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s %s: %w", opts.Binary, strings.Join(args, " "), err)
	}

	b := &Bootstrap{cmd: cmd, out: out, finished: make(chan struct{})}
	go func() {
		b.waitErr = cmd.Wait()
		close(b.finished)
	}()
	return b, nil
}

// WaitForMilestone blocks until milestone appears in the output. It reports an
// error when the command exits first, because a milestone the run has already
// passed the point of will never arrive, and when ctx is done.
func (b *Bootstrap) WaitForMilestone(ctx context.Context, milestone string, poll time.Duration) error {
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if strings.Contains(b.Output(), milestone) {
			return nil
		}
		select {
		case <-b.finished:
			// One last look: the milestone may have been written between the
			// check above and the command exiting.
			if strings.Contains(b.Output(), milestone) {
				return nil
			}
			if b.waitErr != nil {
				return fmt.Errorf("bootstrap site exited before it logged %q: %w", milestone, b.waitErr)
			}
			return fmt.Errorf("bootstrap site exited cleanly before it logged %q", milestone)
		case <-ctx.Done():
			return fmt.Errorf("waiting for %q: %w", milestone, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Wait blocks until the command exits and reports its exit error, nil on a
// clean exit. It is safe to call more than once and after Stop.
func (b *Bootstrap) Wait() error {
	<-b.finished
	return b.waitErr
}

// Exited reports whether the command has already finished.
func (b *Bootstrap) Exited() bool {
	select {
	case <-b.finished:
		return true
	default:
		return false
	}
}

// Output returns everything the command has written so far.
func (b *Bootstrap) Output() string { return b.out.String() }

// stopGrace is how long the process group gets to exit on a terminate signal
// before it is killed. kind and helm clean up after themselves when asked, and
// that cleanup is worth waiting for; a bootstrap wedged in a retry loop is not.
const stopGrace = 20 * time.Second

// Stop signals the command's process group and waits for it. It is a no-op
// once the command has exited, and safe to call more than once.
func (b *Bootstrap) Stop() {
	b.stopOnce.Do(func() {
		if b.Exited() {
			return
		}
		terminateProcessGroup(b.cmd)
		select {
		case <-b.finished:
		case <-time.After(stopGrace):
			killProcessGroup(b.cmd)
		}
	})
	_ = b.Wait()
}

// MissingMilestones returns the milestones absent from out, in the order
// given. An empty result means the run reported every stage.
func MissingMilestones(out string, milestones ...string) []string {
	var missing []string
	for _, m := range milestones {
		if !strings.Contains(out, m) {
			missing = append(missing, m)
		}
	}
	return missing
}

// FindNvkectl resolves the nvkectl binary to exercise: NVKECTL when set,
// otherwise bin/nvkectl under cliDir. It reports an error rather than falling
// back to PATH, so a lane cannot silently test an nvkectl from somewhere else.
func FindNvkectl(cliDir string) (string, error) {
	path := os.Getenv("NVKECTL")
	if path == "" {
		path = cliDir + "/bin/nvkectl"
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("nvkectl %q is not there (%w): build it with `make -C cli build`, or name another in NVKECTL", path, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("nvkectl %q is not an executable file", path)
	}
	return path, nil
}

// syncBuffer is a bytes.Buffer safe for the command's writer goroutine and the
// test goroutine reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// lineWriter forwards whole lines to a Logf, so the test log reads like the
// command's own output rather than being split at arbitrary read boundaries.
type lineWriter struct {
	logf    Logf
	partial []byte
}

func newLineWriter(logf Logf) *lineWriter { return &lineWriter{logf: logf} }

func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.logf("%s", strings.TrimRight(string(w.partial[:i]), "\r"))
		w.partial = w.partial[i+1:]
	}
}
