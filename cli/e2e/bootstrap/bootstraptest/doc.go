// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bootstraptest runs `nvkectl bootstrap site` from a Go test and
// states what the run must have produced.
//
// The e2e lanes for `bootstrap site` grew as two large shell scripts,
// e2e/bootstrap/run.sh and e2e/bootstrap/run-nico.sh, whose assertions are
// greps over a log file. This package moves the part that decides whether a
// run passed into Go, where an assertion can name what it checks and a
// failure can say what it saw.
//
// It owns two things:
//
//   - Bootstrap, which starts the real command, streams its output to the
//     test log, and lets the test act on a milestone while the command is
//     still running. The lanes need that: the bootstrap cluster has to be
//     bridged to the node network after Kind reports it ready and before the
//     controller is deployed.
//   - Cluster, which opens the kubeconfig the command wrote and reports on
//     the cluster behind it.
//
// What it deliberately does not own yet is standing the host up. The
// provisioned lane's node containers already have a Go fixture
// (e2e/bootstrap/substrate), so its test drives them directly. The nico
// lane's nico-sim, profiles image and BGP speaker are still prepared by
// run-nico.sh, and its test takes a prepared host as given. Moving that
// preparation into Go is the next piece; see README.md.
//
// The tests themselves are behind the e2e_bootstrap build tag, so
// `go test ./...` in the merge gate compiles this package and runs its unit
// tests without trying to build a cluster.
package bootstraptest
