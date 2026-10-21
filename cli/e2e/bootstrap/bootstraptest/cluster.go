// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// requestTimeout bounds every API call this package makes. A cluster that has
// just come up can be slow to answer, but not this slow; past it the right
// answer is a failed assertion rather than a test that hangs to its deadline.
const requestTimeout = 30 * time.Second

// Cluster is the site cluster a bootstrap built, reached through the
// kubeconfig the command wrote.
type Cluster struct {
	// KubeconfigPath is the file `bootstrap site --kubeconfig-out` wrote.
	KubeconfigPath string
	// Server is the API server address that kubeconfig points at. With a
	// control-plane VIP this is the VIP, which is the whole point of one.
	Server string

	kube kubernetes.Interface
}

// OpenCluster loads the kubeconfig `bootstrap site` wrote and connects to the
// cluster behind it. The errors are the ones an operator meets: the command
// wrote nothing, wrote an empty file, or wrote something unusable.
func OpenCluster(kubeconfigPath string) (*Cluster, error) {
	info, err := os.Stat(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("bootstrap site wrote no kubeconfig to %s: %w", kubeconfigPath, err)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("bootstrap site wrote an empty kubeconfig to %s", kubeconfigPath)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading the kubeconfig bootstrap site wrote to %s: %w", kubeconfigPath, err)
	}
	cfg.Timeout = requestTimeout
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building a client for %s: %w", kubeconfigPath, err)
	}
	return &Cluster{KubeconfigPath: kubeconfigPath, Server: cfg.Host, kube: kube}, nil
}

// NodeState is one node's name and readiness: the shape the node assertions
// and their failure messages are written against.
type NodeState struct {
	Name  string
	Ready bool
}

// Nodes reports every node the cluster knows, sorted by name so a failure
// message reads the same way every run.
func (c *Cluster) Nodes(ctx context.Context) ([]NodeState, error) {
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the nodes of the cluster at %s: %w", c.Server, err)
	}
	return nodeStates(list.Items), nil
}

// Version reports the Kubernetes version the API server answers with. It is
// the cheapest proof that the cluster is not just described by a file but is
// actually answering.
func (c *Cluster) Version() (string, error) {
	v, err := c.kube.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("asking the API server at %s for its version: %w", c.Server, err)
	}
	return v.GitVersion, nil
}

// WaitNodesReady waits for the cluster to hold exactly want nodes with every
// one of them Ready, and returns the state it settled on.
//
// Count and readiness are checked together on purpose. Waiting only for
// readiness passes the moment the first node is up, before the rest have been
// created, which is how a one-node cluster passes for a three-node one.
func (c *Cluster) WaitNodesReady(ctx context.Context, want int, timeout, poll time.Duration) ([]NodeState, error) {
	if poll <= 0 {
		poll = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var (
		states  []NodeState
		lastErr error
	)
	for {
		states, lastErr = c.Nodes(ctx)
		if lastErr == nil && readyCount(states) == want && len(states) == want {
			return states, nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return states, fmt.Errorf("waiting for %d ready node(s): %w", want, ctx.Err())
		case <-time.After(poll):
		}
	}
	if lastErr != nil {
		return states, fmt.Errorf("waiting for %d ready node(s) in the cluster at %s, last attempt: %w", want, c.Server, lastErr)
	}
	return states, fmt.Errorf("the cluster at %s has %s after %s, want %d node(s), all Ready",
		c.Server, describeNodes(states), timeout, want)
}

// nodeStates reduces the API's nodes to name and readiness, sorted by name.
func nodeStates(nodes []corev1.Node) []NodeState {
	states := make([]NodeState, 0, len(nodes))
	for _, n := range nodes {
		states = append(states, NodeState{Name: n.Name, Ready: isReady(n)})
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	return states
}

// isReady reports whether the node's Ready condition is true. A node with no
// Ready condition at all is not ready: it has registered and nothing more.
func isReady(node corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// readyCount counts the ready nodes.
func readyCount(states []NodeState) int {
	n := 0
	for _, s := range states {
		if s.Ready {
			n++
		}
	}
	return n
}

// describeNodes renders the node states for a failure message: the counts
// first, because that is the comparison that failed, then each node by name.
func describeNodes(states []NodeState) string {
	if len(states) == 0 {
		return "no nodes"
	}
	names := make([]string, 0, len(states))
	for _, s := range states {
		status := "NotReady"
		if s.Ready {
			status = "Ready"
		}
		names = append(names, s.Name+"="+status)
	}
	return fmt.Sprintf("%d node(s), %d Ready (%s)", len(states), readyCount(states), strings.Join(names, ", "))
}
