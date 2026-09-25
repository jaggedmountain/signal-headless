// signal-headless: a headless Signal client.
//
//	signal-headless --link [--name NAME]     link this host as a Signal device (QR)
//	signal-headless --unlink [--force]       remove this device from the account
//	signal-headless --import-signal-cli      adopt the device already linked by signal-cli
//	signal-headless --daemon                 run the device: receive, store, serve clients
//	signal-headless --shell                  interactive TUI (starts the daemon if needed)
//	signal-headless --send TO -m TEXT [-a FILE]...   one-shot send via the daemon
//
// The daemon is the only process that talks to Signal. Everything else is a
// client of its unix socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"signal-headless/internal/paths"
)

var version = "dev"

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type options struct {
	link, unlink, force, daemon, shell, importCLI, status, showVersion bool

	sendTo      string
	message     string
	attachments stringList

	name         string
	dataDir      string
	socket       string
	signalCLIDir string
	account      string
	verbose      bool
	dryRun       bool
	fake         bool
	foreground   bool
}

func parseFlags(args []string) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("signal-headless", flag.ContinueOnError)
	fs.BoolVar(&o.link, "link", false, "link this host as a new Signal device (shows a QR code)")
	fs.BoolVar(&o.unlink, "unlink", false, "remove this device from the Signal account and delete its keys (history is kept)")
	fs.BoolVar(&o.force, "force", false, "--unlink: only delete the local keys, without contacting Signal")
	fs.BoolVar(&o.importCLI, "import-signal-cli", false, "import the device already linked by signal-cli (read-only)")
	fs.BoolVar(&o.daemon, "daemon", false, "run the linked-device daemon")
	fs.BoolVar(&o.shell, "shell", false, "interactive terminal UI")
	fs.BoolVar(&o.status, "status", false, "print daemon/account status")
	fs.BoolVar(&o.showVersion, "version", false, "print version")
	fs.StringVar(&o.sendTo, "send", "", "send a message to `RECIPIENT` (+E164, UUID, group ID, contact name, or 'self')")
	fs.StringVar(&o.message, "m", "", "message text for --send (default: read stdin)")
	fs.Var(&o.attachments, "a", "attachment `FILE` for --send (repeatable)")
	fs.StringVar(&o.name, "name", defaultDeviceName(), "device name shown on the phone (--link)")
	fs.StringVar(&o.dataDir, "data", "", "data directory (default ~/.local/share/signal-headless)")
	fs.StringVar(&o.socket, "socket", "", "daemon socket path (default $XDG_RUNTIME_DIR/signal-headless.sock)")
	fs.StringVar(&o.signalCLIDir, "signal-cli-dir", "", "signal-cli data dir for --import-signal-cli (default ~/.local/share/signal-cli)")
	fs.StringVar(&o.account, "account", "", "signal-cli account number to import, when it holds several")
	fs.BoolVar(&o.dryRun, "dry-run", false, "--import-signal-cli: validate into a scratch database only")
	fs.BoolVar(&o.verbose, "v", false, "verbose logging")
	fs.BoolVar(&o.fake, "fake", false, "daemon: use an in-memory fake Signal backend (development)")
	fs.BoolVar(&o.foreground, "foreground", true, "daemon: log to stderr (false: log to data dir)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: signal-headless [--link | --unlink [--force] | --import-signal-cli | --daemon | --shell | --send TO | --status]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	modes := 0
	for _, b := range []bool{o.link, o.unlink, o.importCLI, o.daemon, o.shell, o.status, o.showVersion, o.sendTo != ""} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		return nil, errors.New("choose one of --link, --unlink, --import-signal-cli, --daemon, --shell, --send, --status, --version")
	}
	if modes == 0 {
		o.shell = true
	}
	return o, nil
}

func defaultDeviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "signal-headless"
	}
	return "signal-headless@" + h
}

// isBridgeInvocation reports a signal-cli style "... jsonRpc" command line.
func isBridgeInvocation(args []string) bool {
	for _, a := range args {
		if a == "jsonRpc" {
			return true
		}
	}
	return false
}

func main() {
	if isBridgeInvocation(os.Args[1:]) {
		// signal-cli flags (--config, -a) are accepted and ignored.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		o := &options{foreground: true}
		if err := runBridge(ctx, o, paths.Resolve("", "")); err != nil {
			fmt.Fprintln(os.Stderr, "signal-headless:", err)
			os.Exit(1)
		}
		return
	}
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "signal-headless:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := resolvePaths(o)
	switch {
	case o.showVersion:
		fmt.Println("signal-headless", version)
	case o.link:
		err = runLink(ctx, o, p)
	case o.unlink:
		err = runUnlink(ctx, o, p)
	case o.importCLI:
		err = runImport(ctx, o, p)
	case o.daemon:
		err = runDaemon(ctx, o, p)
	case o.status:
		err = runStatus(ctx, o, p)
	case o.sendTo != "":
		err = runSend(ctx, o, p)
	case o.shell:
		err = runShell(ctx, o, p)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "signal-headless:", err)
		os.Exit(1)
	}
}
