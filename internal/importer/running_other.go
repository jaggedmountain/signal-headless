// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package importer

// SignalCLIRunning finds nothing outside Linux (no /proc): there, stopping
// signal-cli before importing and running the daemon is up to the user.
func SignalCLIRunning() []int { return nil }
