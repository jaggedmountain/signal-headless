// Package paths resolves where signal-headless keeps its state.
//
// Layout (all overridable with --data / SIGNAL_HEADLESS_DATA):
//
//	$XDG_DATA_HOME/signal-headless/
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
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(home(), ".local", "share")
		}
		p.DataDir = filepath.Join(base, appName)
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

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}
