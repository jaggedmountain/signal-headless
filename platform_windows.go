// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

// detach starts cmd without a console, in its own process group, so it
// outlives the terminal that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

// helperScript is where install.ps1 puts its scripts: NAME.ps1 next to the
// .exe.
func helperScript(exe, name string) string {
	return filepath.Join(filepath.Dir(exe), name+".ps1")
}

// runUpdateScript runs the updater and waits. Replacing this .exe while it
// runs works: install.ps1 renames it first, which Windows allows.
func runUpdateScript(script string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runUninstallScript prints the command instead of running it: the
// uninstaller deletes this .exe, which Windows doesn't allow while it runs.
func runUninstallScript(script string, retain bool) error {
	if retain {
		fmt.Printf("To uninstall, keeping the message history and keys, run in PowerShell:\n\n  & '%s' -Retain\n", script)
		return nil
	}
	fmt.Printf("To uninstall, run in PowerShell:\n\n  & '%s'\n\n"+
		"It unlinks this computer from the Signal account and deletes its message\n"+
		"history and keys (to keep them: -Retain).\n", script)
	return nil
}
