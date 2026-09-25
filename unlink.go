// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"signal-headless/internal/paths"
	"signal-headless/internal/rpc"
	"signal-headless/internal/signalbackend"
)

// confirmNumber asks the user to type the account number.
func confirmNumber(what, number string) error {
	fmt.Println(what)
	fmt.Printf("Type the account number (%s) to confirm: ", number)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("not confirmed")
	}
	if strings.TrimSpace(line) != number {
		return fmt.Errorf("not confirmed; nothing changed")
	}
	return nil
}

func runUnlink(ctx context.Context, o *options, p paths.Paths) error {
	if o.force {
		return forgetLocal(ctx, o, p)
	}
	c, err := connect(ctx, o, p, true)
	if err != nil {
		return fmt.Errorf("%w\n(if the phone already removed this device, use --unlink --force to delete the local keys)", err)
	}
	defer c.Close()
	var st rpc.StatusResult
	if err := c.Call(ctx, rpc.MStatus, nil, &st); err != nil {
		return err
	}
	what := fmt.Sprintf("This removes device %d from the Signal account %s (as if unlinked on the phone)\n"+
		"and deletes its keys here. Message history in %s is kept.", st.Account.DeviceID, st.Account.Number, p.DataDir)
	if err := confirmNumber(what, st.Account.Number); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := c.Call(cctx, rpc.MUnlink, rpc.UnlinkParams{Number: st.Account.Number}, nil); err != nil {
		return fmt.Errorf("unlink failed: %w\n(if the phone already removed this device, use --unlink --force)", err)
	}
	fmt.Println("Unlinked. The daemon has stopped; run --link to link this host again.")
	return nil
}

// forgetLocal deletes the stored device without contacting Signal, for when
// the phone already removed it. The daemon must not be running.
func forgetLocal(ctx context.Context, o *options, p paths.Paths) error {
	if err := p.Ensure(); err != nil {
		return err
	}
	lock, err := lockDataDir(p)
	if err != nil {
		return fmt.Errorf("%w; stop the daemon first", err)
	}
	defer lock.Close()
	d, err := openDB(ctx, o, p)
	if err != nil {
		return err
	}
	defer d.Close()
	dev, err := signalbackend.LoadDevice(ctx, d)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("This deletes the keys of device %d (%s) here WITHOUT telling Signal.\n"+
		"Use it only if the device was already removed on the phone. Message history is kept.", dev.DeviceID, dev.Number)
	if err := confirmNumber(what, dev.Number); err != nil {
		return err
	}
	if err := signalbackend.ForgetDevice(ctx, d); err != nil {
		return err
	}
	fmt.Println("Local device keys deleted; run --link to link this host again.")
	return nil
}
