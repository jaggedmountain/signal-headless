// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"signal-headless/internal/db"
	"signal-headless/internal/importer"
	"signal-headless/internal/paths"
)

func signalCLIDir(o *options) string {
	if o.signalCLIDir != "" {
		return o.signalCLIDir
	}
	// Pinned like our own data dir; see paths.Resolve.
	return filepath.Join(paths.Home(), ".local", "share", "signal-cli")
}

func runImport(ctx context.Context, o *options, p paths.Paths) error {
	log := newLogger(nil, o.verbose)
	cliDir := signalCLIDir(o)
	if o.dryRun {
		// Import into a private scratch database to validate every record,
		// then throw it away. Safe while signal-cli is running (read-only).
		tmp, err := os.MkdirTemp("", "signal-headless-dryrun-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		d, err := db.Open(ctx, filepath.Join(tmp, "dry.db"), log.Level(quietLevel(o)))
		if err != nil {
			return err
		}
		defer d.Close()
		rep, err := importer.Import(ctx, d, cliDir, o.account, log)
		if err != nil {
			return err
		}
		fmt.Println("dry run OK:", rep)
		return nil
	}
	if pids := importer.SignalCLIRunning(); len(pids) > 0 {
		return fmt.Errorf("signal-cli is running (pids %v); stop it first (e.g. systemctl --user stop signal_agent.service)", pids)
	}
	d, err := openDB(ctx, o, p)
	if err != nil {
		return err
	}
	defer d.Close()
	rep, err := importer.Import(ctx, d, cliDir, o.account, log)
	if err != nil {
		return err
	}
	fmt.Println("imported:", rep)
	fmt.Println("signal-cli must not be used for this account from now on.")
	fmt.Println("Start the daemon with: signal-headless --daemon")
	return nil
}
