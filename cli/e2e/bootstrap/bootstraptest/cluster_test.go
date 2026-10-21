// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// node builds a Node with the Ready condition the test wants. A nil ready
// means the node has no Ready condition at all: registered and nothing more.
func node(name string, ready *bool) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if ready != nil {
		status := corev1.ConditionFalse
		if *ready {
			status = corev1.ConditionTrue
		}
		n.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			{Type: corev1.NodeReady, Status: status},
		}
	}
	return n
}

func boolPtr(b bool) *bool { return &b }

// TestNodeStates: readiness is read from the Ready condition, not from any
// other condition that happens to be first, and the result is sorted by name
// so a failure message reads the same way every run.
func TestNodeStates(t *testing.T) {
	states := nodeStates([]corev1.Node{
		node("cp3", boolPtr(false)),
		node("cp1", boolPtr(true)),
		node("cp2", nil),
	})
	assert.Equal(t, []NodeState{
		{Name: "cp1", Ready: true},
		{Name: "cp2", Ready: false},
		{Name: "cp3", Ready: false},
	}, states)
}

func TestReadyCountAndDescribeNodes(t *testing.T) {
	tests := []struct {
		name   string
		states []NodeState
		ready  int
		want   string
	}{
		{"none at all", nil, 0, "no nodes"},
		{
			"a partly ready control plane",
			[]NodeState{{Name: "cp1", Ready: true}, {Name: "cp2", Ready: false}},
			1,
			"2 node(s), 1 Ready (cp1=Ready, cp2=NotReady)",
		},
		{
			"all ready",
			[]NodeState{{Name: "cp1", Ready: true}},
			1,
			"1 node(s), 1 Ready (cp1=Ready)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.ready, readyCount(tt.states))
			assert.Equal(t, tt.want, describeNodes(tt.states))
		})
	}
}

// TestWaitNodesReady_CountAndReadinessTogether: the wait does not pass on
// readiness alone. A one-node cluster must not satisfy a three-node site just
// because its only node is Ready.
func TestWaitNodesReady_CountAndReadinessTogether(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []corev1.Node
		want    int
		wantErr string
	}{
		{
			"exactly what was asked for",
			[]corev1.Node{node("cp1", boolPtr(true)), node("cp2", boolPtr(true)), node("cp3", boolPtr(true))},
			3, "",
		},
		{
			"one Ready node is not a three-node control plane",
			[]corev1.Node{node("cp1", boolPtr(true))},
			3, "1 node(s), 1 Ready (cp1=Ready)",
		},
		{
			"every node present but one not Ready",
			[]corev1.Node{node("cp1", boolPtr(true)), node("cp2", boolPtr(false))},
			2, "2 node(s), 1 Ready (cp1=Ready, cp2=NotReady)",
		},
		{
			"more nodes than the site asked for",
			[]corev1.Node{node("cp1", boolPtr(true)), node("cp2", boolPtr(true))},
			1, "2 node(s), 2 Ready",
		},
		{
			"an empty cluster",
			nil, 1, "no nodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tt.nodes))
			for i := range tt.nodes {
				objs = append(objs, &tt.nodes[i])
			}
			c := &Cluster{Server: "https://192.0.2.1:6443", kube: fake.NewSimpleClientset(objs...)}

			// A short timeout and poll: the fake answers at once, so a failing
			// case needs only one round.
			states, err := c.WaitNodesReady(context.Background(), tt.want, 10*time.Millisecond, time.Millisecond)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Len(t, states, tt.want)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), "https://192.0.2.1:6443",
				"the failure should name the cluster it asked")
		})
	}
}

// TestWaitNodesReady_PassesOnceTheLastNodeJoins: the wait keeps looking rather
// than failing on the first look, which is the whole reason it is a wait.
func TestWaitNodesReady_PassesOnceTheLastNodeJoins(t *testing.T) {
	client := fake.NewSimpleClientset(&[]corev1.Node{node("cp1", boolPtr(true))}[0])
	c := &Cluster{Server: "https://192.0.2.1:6443", kube: client}

	go func() {
		time.Sleep(20 * time.Millisecond)
		second := node("cp2", boolPtr(true))
		_, _ = client.CoreV1().Nodes().Create(context.Background(), &second, metav1.CreateOptions{})
	}()

	states, err := c.WaitNodesReady(context.Background(), 2, 2*time.Second, time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, []NodeState{{Name: "cp1", Ready: true}, {Name: "cp2", Ready: true}}, states)
}

// TestWaitNodesReady_HonoursCancellation: a cancelled context stops the wait
// rather than running it out to the timeout.
func TestWaitNodesReady_HonoursCancellation(t *testing.T) {
	c := &Cluster{Server: "https://192.0.2.1:6443", kube: fake.NewSimpleClientset()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := c.WaitNodesReady(ctx, 1, time.Minute, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled")
	assert.Less(t, time.Since(start), 10*time.Second, "a cancelled wait must not run to its timeout")
}

// TestOpenCluster_ReportsWhatTheCommandDidNotWrite: each failure names the
// path, because the first question is always which file was expected.
func TestOpenCluster_ReportsWhatTheCommandDidNotWrite(t *testing.T) {
	dir := t.TempDir()

	t.Run("no kubeconfig at all", func(t *testing.T) {
		path := filepath.Join(dir, "absent.kubeconfig")
		_, err := OpenCluster(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wrote no kubeconfig")
		assert.Contains(t, err.Error(), path)
	})

	t.Run("an empty kubeconfig", func(t *testing.T) {
		path := filepath.Join(dir, "empty.kubeconfig")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		_, err := OpenCluster(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wrote an empty kubeconfig")
	})

	t.Run("a kubeconfig that will not parse", func(t *testing.T) {
		path := filepath.Join(dir, "garbage.kubeconfig")
		require.NoError(t, os.WriteFile(path, []byte("{{ not a kubeconfig"), 0o600))
		_, err := OpenCluster(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), path)
	})
}

// TestOpenCluster_ReadsTheServerAddress: with a control-plane VIP the written
// kubeconfig must point at the VIP, so the lane reads it back from here.
func TestOpenCluster_ReadsTheServerAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
  - name: site
    cluster:
      server: https://172.26.1.1:6443
      insecure-skip-tls-verify: true
contexts:
  - name: site
    context:
      cluster: site
      user: site
current-context: site
users:
  - name: site
    user: {}
`), 0o600))

	c, err := OpenCluster(path)
	require.NoError(t, err)
	assert.Equal(t, "https://172.26.1.1:6443", c.Server)
	assert.Equal(t, path, c.KubeconfigPath)
}
