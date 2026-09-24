package main

import (
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"

	"signal-headless/internal/db"
	"signal-headless/internal/paths"
	"signal-headless/internal/signalbackend"
)

func openDB(ctx context.Context, o *options, p paths.Paths) (*db.DB, error) {
	if err := p.Ensure(); err != nil {
		return nil, err
	}
	log := newLogger(nil, o.verbose)
	if !o.verbose {
		log = log.Level(zerolog.WarnLevel)
	}
	return db.Open(ctx, p.DB(), log)
}

func runLink(ctx context.Context, o *options, p paths.Paths) error {
	d, err := openDB(ctx, o, p)
	if err != nil {
		return err
	}
	defer d.Close()
	if dev, err := signalbackend.LoadDevice(ctx, d); err == nil {
		return fmt.Errorf("already linked as %s (device %d); remove %s to link again", dev.Number, dev.DeviceID, p.DataDir)
	}
	log := newLogger(nil, o.verbose)
	ctx = log.WithContext(ctx)
	for resp := range signalmeow.PerformProvisioning(ctx, d.Signal, o.name, false) {
		switch resp.State {
		case signalmeow.StateProvisioningURLReceived:
			qr, err := qrcode.New(resp.ProvisioningURL, qrcode.Medium)
			if err != nil {
				return err
			}
			fmt.Println(qr.ToSmallString(false))
			fmt.Println(resp.ProvisioningURL)
			fmt.Println("\nOn the phone: Signal → Settings → Linked devices → Link new device, then scan.")
			fmt.Println("Waiting (2 minutes)…")
		case signalmeow.StateProvisioningDataReceived:
			dd := resp.ProvisioningData
			fmt.Fprintf(os.Stdout, "Linked %s as device %d (%s).\n", dd.Number, dd.DeviceID, o.name)
			fmt.Println("Start the daemon with: signal-headless --daemon")
			return nil
		case signalmeow.StateProvisioningError:
			return fmt.Errorf("linking failed: %w", resp.Err)
		}
	}
	return fmt.Errorf("linking ended without a result")
}
