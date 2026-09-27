// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// detach starts cmd in its own session, away from our terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// helperScript is where install.sh puts its scripts for a binary in
// PREFIX/bin: PREFIX/libexec/signal-headless/NAME.sh.
func helperScript(exe, name string) string {
	return filepath.Join(filepath.Dir(exe), "..", "libexec", "signal-headless", name+".sh")
}

// runUpdateScript and runUninstallScript replace this process with the
// script, which can then replace or delete this binary and ask its
// questions on the terminal.
func runUpdateScript(script string) error {
	return syscall.Exec("/bin/sh", []string{"sh", script}, os.Environ())
}

func runUninstallScript(script string, retain bool) error {
	argv := []string{"sh", script}
	if retain {
		argv = append(argv, "--retain")
	}
	return syscall.Exec("/bin/sh", argv, os.Environ())
}
