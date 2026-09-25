// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"io"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
)

// newLogger logs human-readable lines to w (stderr when nil).
func newLogger(w io.Writer, verbose bool) zerolog.Logger {
	if w == nil {
		w = os.Stderr
	}
	level := zerolog.InfoLevel
	if verbose {
		level = zerolog.DebugLevel
	}
	cw := zerolog.ConsoleWriter{Out: w, TimeFormat: time.DateTime, NoColor: !isTerminal(w)}
	return zerolog.New(cw).Level(level).With().Timestamp().Logger()
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

// quietLevel is the level for chatty subsystems (DB migrations): warnings
// only unless -v.
func quietLevel(o *options) zerolog.Level {
	if o.verbose {
		return zerolog.DebugLevel
	}
	return zerolog.WarnLevel
}
