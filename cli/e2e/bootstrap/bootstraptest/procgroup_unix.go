// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package bootstraptest

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in a process group of its own, so the whole
// tree it starts can be signalled as one.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessGroup asks the command's whole process group to exit.
func terminateProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGTERM) }

// killProcessGroup ends the command's whole process group without asking.
func killProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGKILL) }

// signalProcessGroup sends sig to the group, falling back to the process alone
// when the group is not ours to signal.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// A negative pid is the group. Setpgid made the child its own leader, so
	// the group id is the child's pid.
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}
