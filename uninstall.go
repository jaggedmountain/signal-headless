// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"signal-headless/internal/paths"
)

// installScript finds a script install.sh or install.ps1 wrote for the
// binary we run as ("update" or "uninstall"; helperScript is per OS).
// Empty if there is none: built from source, make install, the VS Code
// extension's copy.
func installScript(name string) (exe, script string, err error) {
	exe, err = os.Executable()
	if err != nil {
		return "", "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	script = helperScript(exe, name)
	if _, err := os.Stat(script); err != nil {
		return exe, "", nil
	}
	return exe, script, nil
}

// runUpdate runs the updater: the latest release's installer with this
// install's options. History, keys and the link are kept.
func runUpdate(p paths.Paths) error {
	exe, script, err := installScript("update")
	if err != nil {
		return err
	}
	if script == "" {
		fmt.Printf("No updater for %s (it wasn't installed by install.sh or install.ps1).\n"+
			"Update it the way it was installed (e.g. rebuild from source), or switch to the installer:\n"+
			"  curl -fsSL https://github.com/jaggedmountain/signal-headless/releases/latest/download/install.sh | sh\n", exe)
		return nil
	}
	return runUpdateScript(script)
}

// runUninstall hands over to the uninstaller. By default it unlinks this
// computer and deletes the message history and keys along with the program;
// retain keeps them. Without an uninstaller it says what to remove.
func runUninstall(p paths.Paths, retain bool) error {
	exe, script, err := installScript("uninstall")
	if err != nil {
		return err
	}
	if script == "" {
		fmt.Printf("No uninstaller for %s (it wasn't installed by install.sh or install.ps1).\n"+
			"To remove it by hand:\n"+
			"  signal-headless --unlink   removes this computer from the Signal account\n"+
			"  signal-headless --stop\n"+
			"then delete %s, and %s: the message history and keys, unencrypted.\n", exe, exe, p.DataDir)
		return nil
	}
	return runUninstallScript(script, retain)
}
