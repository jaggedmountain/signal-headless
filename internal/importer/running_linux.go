// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SignalCLIRunning reports running signal-cli processes (by /proc cmdline).
func SignalCLIRunning() []int {
	var pids []int
	entries, _ := os.ReadDir("/proc")
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(cmd) == 0 {
			continue
		}
		argv0 := string(cmd)
		if i := strings.IndexByte(argv0, 0); i >= 0 {
			argv0 = argv0[:i]
		}
		if filepath.Base(argv0) == "signal-cli" || strings.Contains(string(cmd), "org.asamk.signal.Main") {
			pids = append(pids, pid)
		}
	}
	return pids
}
