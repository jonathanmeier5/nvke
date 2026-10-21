// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ContainerCLI is the container runtime the lanes drive. It matches
// bootstrapengine.RuntimeCLI and substrate.ContainerCLI: one runtime for the
// whole suite, so the node containers and the bootstrap cluster share a daemon.
const ContainerCLI = "nerdctl"

// KindNetwork is the network Kind puts the bootstrap cluster on.
const KindNetwork = "kind"

// BridgeAttachment bridges two nerdctl networks for the length of a run.
//
// Both networks are CNI bridges, and nerdctl's firewall plugin sets
// ingressPolicy "same-bridge", which drops any forward from one bridge out
// another. The controller reaching nvke-agent, and a node reaching the SPIRE
// server on the bootstrap cluster's NodePort, both cross. Without this the
// controller's preflight times out and no node attests, fifteen minutes after
// the point where the cause was visible.
//
// Exactly this pair of bridges is permitted, in both directions, at the front
// of the FORWARD chain so the rules precede the isolation chain. Nothing else
// on the host becomes reachable. Detach removes every rule Attach added.
type BridgeAttachment struct {
	logf  Logf
	undo  []func(context.Context)
	first string
	secnd string
}

// AttachBridges permits forwarding between the two named nerdctl networks, and
// between the second network and itself when sameBridge is set — VIP traffic
// from a node to its own control plane enters and leaves one bridge.
//
// It needs root: it writes iptables rules.
func AttachBridges(ctx context.Context, firstNetwork, secondNetwork string, sameBridge bool, logf Logf) (*BridgeAttachment, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	first, err := BridgeOf(ctx, firstNetwork)
	if err != nil {
		return nil, err
	}
	second, err := BridgeOf(ctx, secondNetwork)
	if err != nil {
		return nil, err
	}

	a := &BridgeAttachment{logf: logf, first: first, secnd: second}
	pairs := [][2]string{{first, second}, {second, first}}
	if sameBridge {
		pairs = append(pairs, [2]string{second, second})
	}
	for _, p := range pairs {
		if err := a.permit(ctx, p[0], p[1]); err != nil {
			a.Detach(ctx)
			return nil, err
		}
	}
	logf("bootstraptest: bridged %s (%s) and %s (%s)", firstNetwork, first, secondNetwork, second)
	return a, nil
}

// permit inserts one ACCEPT rule and registers its removal.
func (a *BridgeAttachment) permit(ctx context.Context, in, out string) error {
	if _, err := run(ctx, "iptables", "-I", "FORWARD", "1", "-i", in, "-o", out, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("permitting forwarding %s -> %s: %w", in, out, err)
	}
	a.undo = append(a.undo, func(ctx context.Context) {
		if _, err := run(ctx, "iptables", "-D", "FORWARD", "-i", in, "-o", out, "-j", "ACCEPT"); err != nil {
			a.logf("bootstraptest: could not remove the %s -> %s forward rule: %v", in, out, err)
		}
	})
	return nil
}

// AddAddress puts addr, in CIDR form, on the second bridge, and registers its
// removal with Detach. The lanes use it for the SPIRE server address the node
// images were built to look for, which has to exist on the node network before
// any agent can reach it.
func (a *BridgeAttachment) AddAddress(ctx context.Context, addr string) error {
	bridge := a.secnd
	if _, err := run(ctx, "ip", "address", "replace", addr, "dev", bridge); err != nil {
		return fmt.Errorf("adding %s to %s: %w", addr, bridge, err)
	}
	a.undo = append(a.undo, func(ctx context.Context) {
		if _, err := run(ctx, "ip", "address", "del", addr, "dev", bridge); err != nil {
			a.logf("bootstraptest: could not remove %s from %s: %v", addr, bridge, err)
		}
	})
	return nil
}

// Detach removes everything Attach and AddAddress put on the host, newest
// first. It reports nothing: it runs on the way out, where the test's own
// failure is the news, and every step logs what it could not undo.
func (a *BridgeAttachment) Detach(ctx context.Context) {
	for i := len(a.undo) - 1; i >= 0; i-- {
		a.undo[i](ctx)
	}
	a.undo = nil
}

// BridgeOf returns the host bridge interface backing a nerdctl network.
func BridgeOf(ctx context.Context, network string) (string, error) {
	out, err := run(ctx, ContainerCLI, "network", "inspect", "--format", "{{.Id}}", network)
	if err != nil {
		return "", fmt.Errorf("reading the id of network %q: %w", network, err)
	}
	bridge, err := bridgeName(out)
	if err != nil {
		return "", fmt.Errorf("network %q: %w", network, err)
	}
	if _, err := run(ctx, "ip", "link", "show", bridge); err != nil {
		return "", fmt.Errorf("network %q has no bridge %s on the host yet: %w", network, bridge, err)
	}
	return bridge, nil
}

// bridgeName derives the bridge interface name from a network id, the way
// nerdctl's CNI config names it: "br-" and the first twelve characters.
func bridgeName(id string) (string, error) {
	id = strings.TrimSpace(id)
	if len(id) < 12 {
		return "", fmt.Errorf("network id %q is too short to name a bridge", id)
	}
	return "br-" + id[:12], nil
}

// run executes a command and returns its combined output, with that output in
// the error when it fails: for iptables and ip, the message is the diagnosis.
func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
