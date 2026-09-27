// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"signal-headless/internal/backend"
	"signal-headless/internal/daemon"
	"signal-headless/internal/db"
	"signal-headless/internal/fakebackend"
	"signal-headless/internal/history"
	"signal-headless/internal/importer"
	"signal-headless/internal/linkpreview"
	"signal-headless/internal/model"
	"signal-headless/internal/paths"
	"signal-headless/internal/rpc"
	"signal-headless/internal/signalbackend"
)

// resolvePaths applies --fake defaults: a scratch data dir and socket, so a
// fake daemon never touches the real account's store.
func resolvePaths(o *options) paths.Paths {
	if o.fake {
		if o.dataDir == "" {
			o.dataDir = filepath.Join(os.TempDir(), fmt.Sprintf("signal-headless-fake-%d", os.Getuid()))
		}
		// Also with --data: the default socket is the real daemon's.
		if o.socket == "" {
			o.socket = filepath.Join(o.dataDir, "signal-headless.sock")
		}
	}
	return paths.Resolve(o.dataDir, o.socket)
}

// errFakeOnRealSocket: a --fake daemon must never answer on the real
// daemon's socket, where clients would take it for the real account.
var errFakeOnRealSocket = errors.New("--fake won't use the default socket (the real daemon's); pass --socket")

// lockDataDir takes an exclusive lock so only one daemon runs per store
// (lockFile is per platform).
func lockDataDir(p paths.Paths) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(p.DataDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("another daemon is using %s", p.DataDir)
	}
	return f, nil
}

func runDaemon(ctx context.Context, o *options, p paths.Paths) error {
	if o.fake && p.Socket == paths.DefaultSocket() {
		return errFakeOnRealSocket
	}
	if err := p.Ensure(); err != nil {
		return err
	}
	var logOut io.Writer = os.Stderr
	if !o.foreground {
		f, err := os.OpenFile(p.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		logOut = f
	}
	log := newLogger(logOut, o.verbose)

	lock, err := lockDataDir(p)
	if err != nil {
		return err
	}
	defer lock.Close()
	if !o.fake {
		if pids := importer.SignalCLIRunning(); len(pids) > 0 {
			return fmt.Errorf("signal-cli is running (pids %v); it must not share this device — stop it first", pids)
		}
	}

	d, err := db.Open(ctx, p.DB(), log.Level(quietLevel(o)))
	if err != nil {
		return err
	}
	defer d.Close()

	var be backend.Backend
	if o.fake {
		f := fakebackend.New()
		f.Echo, f.Seed = true, true
		if os.Getenv("SIGNAL_HEADLESS_DEMO") == "1" {
			f = fakebackend.NewDemo() // the screenshot data set (docs/screenshot.sh)
		}
		be = f
		log.Warn().Str("data", p.DataDir).Msg("Using the FAKE backend; nothing reaches Signal")
	} else {
		sb, err := signalbackend.New(ctx, d, log)
		if err != nil {
			return err
		}
		be = sb
	}
	cfg := daemon.Config{Socket: p.Socket, AttachmentsDir: p.Attachments(), DBPath: p.DB(), Version: version, DeletedTTL: o.deletedTTL}
	if o.fake {
		cfg.LinkPreview = fakebackend.LinkPreview(p.Attachments())
	} else {
		f := &linkpreview.Fetcher{Dir: p.Attachments()}
		cfg.LinkPreview = func(ctx context.Context, url string) (*model.OutgoingPreview, error) {
			lp, err := f.Fetch(ctx, url)
			if err != nil {
				return nil, err
			}
			return &model.OutgoingPreview{URL: lp.URL, Title: lp.Title, Description: lp.Description, Date: lp.Date, Image: lp.Image}, nil
		}
	}
	dmn := daemon.New(cfg, be, history.New(d.Database), log)
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if !o.fake {
		go guardAgainstSignalCLI(runCtx, cancel)
	}
	err = dmn.Run(runCtx)
	if cause := context.Cause(runCtx); err == nil && cause != nil && cause != context.Canceled && ctx.Err() == nil {
		err = cause
	}
	if err != nil {
		// A background daemon's stderr goes nowhere; the log is where to look.
		log.Error().Err(err).Msg("Daemon stopped")
	} else {
		log.Info().Msg("Daemon stopped")
	}
	return err
}

// guardAgainstSignalCLI stops the daemon if signal-cli starts: two clients
// on one device identity split the message queue and corrupt each other's
// sessions. (A still-enabled signal_agent.service would do this at login.)
func guardAgainstSignalCLI(ctx context.Context, stop context.CancelCauseFunc) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if pids := importer.SignalCLIRunning(); len(pids) > 0 {
				stop(fmt.Errorf("signal-cli started (pids %v); stopping to protect the shared device identity", pids))
				return
			}
		}
	}
}

// connect dials the daemon, starting it in the background if needed.
func connect(ctx context.Context, o *options, p paths.Paths, autostart bool) (*rpc.Client, error) {
	c, err := rpc.Dial(p.Socket)
	if err == nil || !autostart {
		if err != nil {
			return nil, fmt.Errorf("daemon not running (%s): %w", p.Socket, err)
		}
		return c, nil
	}
	if err := startDaemon(ctx, o, p); err != nil {
		return nil, err
	}
	return rpc.Dial(p.Socket)
}

func startDaemon(ctx context.Context, o *options, p paths.Paths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"--daemon", "--foreground=false", "--data", p.DataDir, "--socket", p.Socket}
	if o.fake {
		args = append(args, "--fake")
	}
	if o.verbose {
		args = append(args, "-v")
	}
	cmd := exec.Command(exe, args...)
	detach(cmd) // outlives this process and its terminal
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case err := <-exited:
			msg := strings.TrimSpace(stderr.String())
			if msg == "" && err != nil {
				msg = err.Error()
			}
			return fmt.Errorf("daemon failed to start: %s (log: %s)", msg, p.DaemonLog())
		case <-deadline:
			return fmt.Errorf("daemon did not come up within 20s (log: %s)", p.DaemonLog())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			if c, err := net.DialTimeout("unix", p.Socket, time.Second); err == nil {
				c.Close()
				return nil
			}
		}
	}
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if l.n <= 0 {
		return len(b), nil
	}
	k := min(len(b), l.n)
	l.n -= k
	_, _ = l.w.Write(b[:k])
	return len(b), nil
}

func runStatus(ctx context.Context, o *options, p paths.Paths) error {
	c, err := connect(ctx, o, p, false)
	if err != nil {
		return err
	}
	defer c.Close()
	var st rpc.StatusResult
	if err := c.Call(ctx, rpc.MStatus, nil, &st); err != nil {
		return err
	}
	fmt.Printf("account:    %s (device %d)\n", st.Account.Number, st.Account.DeviceID)
	fmt.Printf("connection: %s", st.Connection)
	if st.Error != "" {
		fmt.Printf(" (%s)", st.Error)
	}
	fmt.Printf("\ncaught up:  %v\nclients:    %d\ndaemon:     %s\nsocket:     %s\n", st.QueueEmpty, st.Clients, st.Version, p.Socket)
	return nil
}

func runSend(ctx context.Context, o *options, p paths.Paths) error {
	body := o.message
	if body == "" && len(o.attachments) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		body = strings.TrimRight(string(b), "\n")
	}
	c, err := connect(ctx, o, p, true)
	if err != nil {
		return err
	}
	defer c.Close()
	var res rpc.SendResult
	err = c.Call(ctx, rpc.MSend, rpc.SendParams{To: o.sendTo, Body: body, Attachments: o.attachments}, &res)
	if err != nil {
		return err
	}
	fmt.Println(res.Timestamp)
	return nil
}

// runBridge implements `signal-headless [--config X] [-a ACCOUNT] jsonRpc`:
// it relays stdio to the daemon socket so tools written for signal-cli's
// jsonRpc mode (e.g. signal_agent) can use signal-headless unchanged.
func runBridge(ctx context.Context, o *options, p paths.Paths) error {
	if _, err := connect(ctx, o, p, true); err != nil {
		return err
	}
	conn, err := net.Dial("unix", p.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		// On stdin EOF, half-close so the daemon answers what's in flight
		// and then hangs up, which ends the copy below.
		_, _ = io.Copy(conn, os.Stdin)
		_ = conn.(*net.UnixConn).CloseWrite()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(os.Stdout, conn)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
		return nil
	}
}

// runStop asks the running daemon to exit.
func runStop(ctx context.Context, o *options, p paths.Paths) error {
	c, err := connect(ctx, o, p, false)
	if err != nil {
		fmt.Println("no daemon running")
		return nil
	}
	defer c.Close()
	if err := c.Call(ctx, rpc.MShutdown, nil, nil); err != nil {
		var re *rpc.Error
		if errors.As(err, &re) && re.Code == rpc.CodeMethodNotFound {
			return errors.New("this daemon predates --stop; stop it with: pkill -x signal-headless")
		}
		return err
	}
	fmt.Println("daemon stopping")
	return nil
}
