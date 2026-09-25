// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"signal-headless/internal/fakebackend"
	"signal-headless/internal/paths"
	"signal-headless/internal/rpc"
	"signal-headless/internal/signalbackend"
)

// checkResult is --check --json's output.
type checkResult struct {
	Linked        bool   `json:"linked"`
	Number        string `json:"number,omitempty"`
	DeviceID      int    `json:"deviceId,omitempty"`
	DaemonRunning bool   `json:"daemonRunning"`
	Version       string `json:"version"`
	DataDir       string `json:"dataDir"`
	Socket        string `json:"socket"`
}

// runCheck reports whether a device is linked, asking the daemon when one
// runs (it owns the store) and reading the store otherwise.
func runCheck(ctx context.Context, o *options, p paths.Paths) error {
	r := checkResult{Version: version, DataDir: p.DataDir, Socket: p.Socket}
	if c, err := rpc.Dial(p.Socket); err == nil {
		var st rpc.StatusResult
		err = c.Call(ctx, rpc.MStatus, nil, &st)
		c.Close()
		if err != nil {
			return err
		}
		r.DaemonRunning = true
		r.Linked = st.Account.Number != "" && st.Connection != "logged-out"
		r.Number, r.DeviceID = st.Account.Number, st.Account.DeviceID
	} else if o.fake {
		a := fakebackend.New().Account()
		r.Linked, r.Number, r.DeviceID = true, a.Number, a.DeviceID
	} else {
		if err := p.Ensure(); err != nil {
			return err
		}
		d, err := openDB(ctx, o, p)
		if err != nil {
			return err
		}
		defer d.Close()
		dev, err := signalbackend.LoadDevice(ctx, d)
		switch {
		case err == nil:
			r.Linked, r.Number, r.DeviceID = true, dev.Number, dev.DeviceID
		case err != signalbackend.ErrNoDevice:
			return err
		}
	}
	if o.json {
		if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
			return err
		}
	} else if r.Linked {
		fmt.Printf("linked as %s (device %d)\n", r.Number, r.DeviceID)
	} else {
		fmt.Println("not linked")
	}
	if !r.Linked {
		os.Exit(exitNotLinked)
	}
	return nil
}
