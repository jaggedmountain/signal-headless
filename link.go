package main

import (
	"context"
	"fmt"
	"os"

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
	return db.Open(ctx, p.DB(), newLogger(nil, o.verbose).Level(quietLevel(o)))
}

// printLinkQR shows the provisioning URL as a terminal QR code plus instructions.
func printLinkQR(url string) error {
	qr, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Println(qr.ToSmallString(false))
	fmt.Println(url)
	fmt.Println("\nOn the phone: Signal → Settings → Linked devices → Link new device, then scan.")
	return nil
}

func runLink(ctx context.Context, o *options, p paths.Paths) error {
	if o.fake {
		// Preview only: a dummy URL of the real shape, no network, nothing stored.
		// Scanning it on a phone fails harmlessly (no provisioning session exists).
		if err := printLinkQR("sgnl://linkdevice?uuid=FAKE-preview-not-a-real-session&pub_key=BQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
			return err
		}
		fmt.Println("(--fake: preview of the linking screen; this QR code links nothing)")
		return nil
	}
	d, err := openDB(ctx, o, p)
	if err != nil {
		return err
	}
	defer d.Close()
	if dev, err := signalbackend.LoadDevice(ctx, d); err == nil {
		return fmt.Errorf("already linked as %s (device %d); run --unlink first (message history is kept)", dev.Number, dev.DeviceID)
	}
	log := newLogger(nil, o.verbose)
	ctx = log.WithContext(ctx)
	for resp := range signalmeow.PerformProvisioning(ctx, d.Signal, o.name, false) {
		switch resp.State {
		case signalmeow.StateProvisioningURLReceived:
			if err := printLinkQR(resp.ProvisioningURL); err != nil {
				return err
			}
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
