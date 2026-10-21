// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package bootstraptest

import "os/exec"

// The bootstrap e2e lanes run on Linux. These keep the package building
// elsewhere, so `go build ./...` and the linter work on any developer machine.

// setProcessGroup is a no-op off unix.
func setProcessGroup(*exec.Cmd) {}

// terminateProcessGroup ends the command itself, the best this platform offers.
func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// killProcessGroup ends the command itself.
func killProcessGroup(cmd *exec.Cmd) { terminateProcessGroup(cmd) }
