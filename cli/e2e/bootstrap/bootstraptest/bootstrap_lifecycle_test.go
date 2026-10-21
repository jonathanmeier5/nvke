// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package bootstraptest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNvkectl writes a shell script that stands in for the binary, so the
// runner's own behaviour can be tested without a cluster.
func fakeNvkectl(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nvkectl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755))
	return path
}

func testOptions(binary string) BootstrapOptions {
	return BootstrapOptions{Binary: binary, SiteSpec: "/dev/null"}
}

// TestBootstrap_CompletesAndReportsItsMilestones: the output is captured whole,
// whatever order the writes arrived in, and a clean exit is a nil Wait.
func TestBootstrap_CompletesAndReportsItsMilestones(t *testing.T) {
	binary := fakeNvkectl(t, `
echo "bootstrap kind cluster ready"
echo "controller deployment healthy" >&2
echo "cluster provisioned"
exit 0
`)
	b, err := StartBootstrap(context.Background(), testOptions(binary), t.Logf)
	require.NoError(t, err)
	defer b.Stop()

	require.NoError(t, b.Wait())
	assert.Empty(t, MissingMilestones(b.Output(),
		MilestoneKindClusterReady, MilestoneControllerHealthy, MilestoneClusterProvisioned),
		"stderr as well as stdout must be captured")
}

// TestBootstrap_WaitForMilestoneReturnsWhileTheCommandRuns: the lanes bridge
// their networks at a milestone, with the command still going. A wait that
// only returned at exit would be useless for that.
func TestBootstrap_WaitForMilestoneReturnsWhileTheCommandRuns(t *testing.T) {
	done := filepath.Join(t.TempDir(), "released")
	binary := fakeNvkectl(t, `
echo "bootstrap kind cluster ready"
while [ ! -f `+strconv.Quote(done)+` ]; do sleep 0.05; done
echo "cluster provisioned"
exit 0
`)
	b, err := StartBootstrap(context.Background(), testOptions(binary), t.Logf)
	require.NoError(t, err)
	defer b.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, b.WaitForMilestone(ctx, MilestoneKindClusterReady, 10*time.Millisecond),
		"the milestone must be seen before the command exits")

	// Still running: the command is waiting on us, which is the point.
	assert.Contains(t, MissingMilestones(b.Output(), MilestoneClusterProvisioned), MilestoneClusterProvisioned)

	require.NoError(t, os.WriteFile(done, nil, 0o600))
	require.NoError(t, b.Wait())
	assert.Empty(t, MissingMilestones(b.Output(), MilestoneClusterProvisioned))
}

// TestBootstrap_WaitForMilestoneFailsWhenTheCommandExitsFirst: a milestone the
// run is already past will never arrive, so the wait says so instead of
// running to its deadline.
func TestBootstrap_WaitForMilestoneFailsWhenTheCommandExitsFirst(t *testing.T) {
	binary := fakeNvkectl(t, `
echo "standing up the bootstrap cluster"
exit 3
`)
	b, err := StartBootstrap(context.Background(), testOptions(binary), t.Logf)
	require.NoError(t, err)
	defer b.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = b.WaitForMilestone(ctx, MilestoneKindClusterReady, 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), MilestoneKindClusterReady)
	assert.Contains(t, err.Error(), "exited")

	// The exit status is still the caller's to read: WaitForMilestone must not
	// have consumed it.
	require.Error(t, b.Wait())
	assert.Contains(t, b.Wait().Error(), "exit status 3")
}

// TestBootstrap_StopEndsTheWholeProcessGroup: nvkectl does not forward signals
// to the kind, helm and nerdctl it drives. Stopping only the direct child
// would leave those running and the next run would trip over what they hold.
func TestBootstrap_StopEndsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	childPID := filepath.Join(dir, "child.pid")
	binary := fakeNvkectl(t, `
sleep 300 &
echo $! > `+strconv.Quote(childPID)+`
echo "bootstrap kind cluster ready"
wait
`)
	b, err := StartBootstrap(context.Background(), testOptions(binary), t.Logf)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, b.WaitForMilestone(ctx, MilestoneKindClusterReady, 10*time.Millisecond))

	raw, err := os.ReadFile(childPID)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	require.NoError(t, syscall.Kill(pid, 0), "the grandchild should be running before Stop")

	b.Stop()

	// The signal is delivered to the group; the kernel reaps asynchronously,
	// so allow a moment before concluding it survived.
	assert.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 10*time.Second, 20*time.Millisecond, "Stop must end the grandchild, not just the direct child")
}

// TestBootstrap_StopIsSafeAfterTheCommandExited: the lanes defer Stop, so it
// runs on the success path too, and may run twice.
func TestBootstrap_StopIsSafeAfterTheCommandExited(t *testing.T) {
	b, err := StartBootstrap(context.Background(), testOptions(fakeNvkectl(t, "exit 0\n")), t.Logf)
	require.NoError(t, err)

	require.NoError(t, b.Wait())
	b.Stop()
	b.Stop()
	assert.NoError(t, b.Wait(), "the exit status must survive Stop")
}

// TestBootstrap_StopAfterWaitIsImmediate: the lanes defer Stop and also Wait
// for the exit status, so Stop runs on an already-finished command on every
// successful run. It must not sit out the grace period there -- that turned a
// one-second test into a twenty-second one, and would add the same to every
// lane.
func TestBootstrap_StopAfterWaitIsImmediate(t *testing.T) {
	b, err := StartBootstrap(context.Background(), testOptions(fakeNvkectl(t, "exit 0\n")), t.Logf)
	require.NoError(t, err)
	require.NoError(t, b.Wait())
	require.True(t, b.Exited())

	start := time.Now()
	b.Stop()
	assert.Less(t, time.Since(start), stopGrace/2,
		"Stop on a command that already exited must return at once")
}

// TestBootstrap_RefusesIncompleteOptions: nothing is started when the options
// cannot name what to run.
func TestBootstrap_RefusesIncompleteOptions(t *testing.T) {
	_, err := StartBootstrap(context.Background(), BootstrapOptions{SiteSpec: "s"}, t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Binary is empty")
}

// TestBootstrap_PassesTheEnvironmentThrough: the lanes hand the command its
// controller image and credentials paths this way.
func TestBootstrap_PassesTheEnvironmentThrough(t *testing.T) {
	binary := fakeNvkectl(t, "echo \"image=$NVKECTL_NVKE_CONTROLLER_IMAGE\"\n")
	opts := testOptions(binary)
	opts.Env = []string{"NVKECTL_NVKE_CONTROLLER_IMAGE=nke-controller:e2e"}

	b, err := StartBootstrap(context.Background(), opts, t.Logf)
	require.NoError(t, err)
	defer b.Stop()
	require.NoError(t, b.Wait())
	assert.Contains(t, b.Output(), "image=nke-controller:e2e")
}
