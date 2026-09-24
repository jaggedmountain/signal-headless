// Package paths resolves where signal-headless keeps its state.
//
// Layout (all overridable with --data / SIGNAL_HEADLESS_DATA):
//
//	~/.local/share/signal-headless/
//	  signal-headless.db     signalmeow keys + sessions, message history
//	  attachments/           downloaded attachments
//	  daemon.log             daemon log when auto-started by --shell
//	$XDG_RUNTIME_DIR/signal-headless.sock   daemon socket
package paths

import (
	"os"
	"path/filepath"
)

const appName = "signal-headless"

type Paths struct {
	DataDir string
	Socket  string
}

// Resolve returns the paths to use. dataOverride and socketOverride win over
// the environment, which wins over XDG defaults.
func Resolve(dataOverride, socketOverride string) Paths {
	p := Paths{DataDir: dataOverride, Socket: socketOverride}
	if p.DataDir == "" {
		p.DataDir = os.Getenv("SIGNAL_HEADLESS_DATA")
	}
	if p.DataDir == "" {
		// XDG_DATA_HOME is deliberately ignored: confined terminals (e.g. the
		// VS Code snap) point it at a private directory, which would split the
		// daemon and its clients across two stores.
		p.DataDir = filepath.Join(home(), ".local", "share", appName)
	}
	if p.Socket == "" {
		p.Socket = os.Getenv("SIGNAL_HEADLESS_SOCKET")
	}
	if p.Socket == "" {
		if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
			p.Socket = filepath.Join(rt, appName+".sock")
		} else {
			p.Socket = filepath.Join(p.DataDir, appName+".sock")
		}
	}
	return p
}

func (p Paths) DB() string          { return filepath.Join(p.DataDir, appName+".db") }
func (p Paths) Attachments() string { return filepath.Join(p.DataDir, "attachments") }
func (p Paths) DaemonLog() string   { return filepath.Join(p.DataDir, "daemon.log") }

// Ensure creates the data and attachment directories with private permissions.
func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.Attachments(), 0o700); err != nil {
		return err
	}
	return os.Chmod(p.DataDir, 0o700)
}

// Home returns the user's home directory ("." if unknown).
func Home() string { return home() }

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}
