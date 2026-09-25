// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"

	"signal-headless/internal/db"
	"signal-headless/internal/fakebackend"
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

// linkEvent is one line of --link --json output.
type linkEvent struct {
	Event    string `json:"event"` // "url", "linked", "error"
	URL      string `json:"url,omitempty"`
	Number   string `json:"number,omitempty"`
	DeviceID int    `json:"deviceId,omitempty"`
	Name     string `json:"name,omitempty"`
	Code     string `json:"code,omitempty"` // error: "already-linked", "failed"
	Message  string `json:"message,omitempty"`
	Fake     bool   `json:"fake,omitempty"`
}

func emit(e linkEvent) {
	_ = json.NewEncoder(os.Stdout).Encode(e)
}

func runLink(ctx context.Context, o *options, p paths.Paths) error {
	fakeURL := "sgnl://linkdevice?uuid=FAKE-preview-not-a-real-session&pub_key=BQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if o.fake && o.json {
		// Development: the linking conversation without Signal. Nothing is stored.
		emit(linkEvent{Event: "url", URL: fakeURL, Fake: true})
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		a := fakebackend.New().Account()
		emit(linkEvent{Event: "linked", Number: a.Number, DeviceID: a.DeviceID, Name: o.name, Fake: true})
		return nil
	}
	if o.fake {
		// Preview only: a dummy URL of the real shape, no network, nothing stored.
		// Scanning it on a phone fails harmlessly (no provisioning session exists).
		if err := printLinkQR(fakeURL); err != nil {
			return err
		}
		fmt.Println("(--fake: preview of the linking screen; this QR code links nothing)")
		return nil
	}
	fail := func(code string, err error) error {
		if o.json {
			emit(linkEvent{Event: "error", Code: code, Message: err.Error()})
		}
		return err
	}
	if err := p.Ensure(); err != nil {
		return fail("failed", err)
	}
	d, err := openDB(ctx, o, p)
	if err != nil {
		return fail("failed", err)
	}
	defer d.Close()
	if dev, err := signalbackend.LoadDevice(ctx, d); err == nil {
		return fail("already-linked", fmt.Errorf("already linked as %s (device %d); run --unlink first (message history is kept)", dev.Number, dev.DeviceID))
	}
	log := newLogger(nil, o.verbose) // stderr; stdout carries the QR code or JSON
	ctx = log.WithContext(ctx)
	// allowBackup: advertise "link and sync", so the phone offers to
	// transfer message history; the daemon receives it after linking.
	for resp := range signalmeow.PerformProvisioning(ctx, d.Signal, o.name, true) {
		switch resp.State {
		case signalmeow.StateProvisioningURLReceived:
			if o.json {
				emit(linkEvent{Event: "url", URL: resp.ProvisioningURL})
				continue
			}
			if err := printLinkQR(resp.ProvisioningURL); err != nil {
				return err
			}
			fmt.Println("Waiting (2 minutes)…")
		case signalmeow.StateProvisioningDataReceived:
			dd := resp.ProvisioningData
			if o.json {
				emit(linkEvent{Event: "linked", Number: dd.Number, DeviceID: dd.DeviceID, Name: o.name})
				return nil
			}
			fmt.Fprintf(os.Stdout, "Linked %s as device %d (%s).\n", dd.Number, dd.DeviceID, o.name)
			fmt.Println("Start the daemon with: signal-headless --daemon")
			fmt.Println("If the phone offered \"Transfer message history\", the daemon imports it once started.")
			return nil
		case signalmeow.StateProvisioningError:
			return fail("failed", fmt.Errorf("linking failed: %w", resp.Err))
		}
	}
	return fail("failed", fmt.Errorf("linking ended without a result"))
}
