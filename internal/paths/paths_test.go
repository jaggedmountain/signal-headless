// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package paths

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaults(t *testing.T) {
	t.Setenv("SIGNAL_HEADLESS_DATA", "")
	t.Setenv("SIGNAL_HEADLESS_SOCKET", "")
	t.Setenv("XDG_DATA_HOME", "/snap/private/share") // ignored on purpose
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	p := Resolve("", "")
	if runtime.GOOS == "linux" && p.DataDir != filepath.Join(home(), ".local", "share", "signal-headless") {
		t.Fatalf("data dir = %s", p.DataDir)
	}
	if p.Socket != "/run/user/1234/signal-headless.sock" {
		t.Fatalf("socket = %s", p.Socket)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if p := Resolve("", ""); p.Socket != filepath.Join(p.DataDir, "signal-headless.sock") {
		t.Fatalf("socket without XDG_RUNTIME_DIR = %s", p.Socket)
	}
	if p := Resolve("/d", "/s.sock"); p.DataDir != "/d" || p.Socket != "/s.sock" {
		t.Fatalf("overrides = %+v", p)
	}
}
