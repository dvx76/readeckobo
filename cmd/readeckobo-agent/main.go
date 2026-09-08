// Command readeckobo-agent is the on-device sync agent for the readeckobo
// project. It runs on a Kobo Libra 2 (firmware 4.38.x, linux/arm GOARM=7),
// downloads kepubs served by the readeckobo server into Nickel's library
// (via .kobo/readeck/ + a NickelDBus rescan), files them into a "Readeck"
// collection, and uploads new highlights/notes read from KoboReader.sqlite.
//
// It is built as a single static binary (CGO_ENABLED=0) so it runs on the
// stock firmware without any toolchain on the device. See docs/AGENT.md for
// installation and troubleshooting, and docs/research/kobo-db-schema.md for
// the device-side research this implementation is based on.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// On-device defaults. Everything is overridable via flags (and QNDB/HTTP
// settings via the config file) so tests and manual runs can point at a
// temporary tree.
const (
	defaultConfigPath = "/mnt/onboard/.adds/readeckobo/config"
	defaultOnboard    = "/mnt/onboard"
	defaultKoboDBPath = "/mnt/onboard/.kobo/KoboReader.sqlite"
	defaultInterval   = 30 * time.Minute
)

// cliOptions mirrors the flag set (kept separate so runCLI is testable).
type cliOptions struct {
	configPath string
	once       bool
	interval   time.Duration
	koboDB     string
	onboard    string
	noRescan   bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runCLI(ctx, os.Args[1:], os.Stderr))
}

func runCLI(ctx context.Context, args []string, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return 2
	}

	cfg, err := loadConfig(opts.configPath)
	if err != nil {
		fmt.Fprintf(stderr, "readeckobo-agent: %v\n", err)
		return 2
	}

	dataDir := filepath.Dir(opts.configPath)
	logger, err := newLogger(filepath.Join(dataDir, "agent.log"), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "readeckobo-agent: cannot open log: %v\n", err)
		return 2
	}

	// Single-instance lock: the udev hook, NickelMenu entries and the loop
	// mode may otherwise race on the index/ledger/DB.
	lock, err := acquireInstanceLock(filepath.Join(dataDir, "agent.lock"))
	if err != nil {
		logger.Error("another instance is running (%v); exiting", err)
		return 1
	}
	defer lock.release()

	ag, err := newAgent(agentOptions{
		cfg:      cfg,
		onboard:  opts.onboard,
		koboDB:   opts.koboDB,
		dataDir:  dataDir,
		noRescan: opts.noRescan,
		logger:   logger,
		snapshot: "/tmp/readeckobo-agent.sqlite",
		pollWait: 2 * time.Second,
		pollTime: 180 * time.Second,
	})
	if err != nil {
		logger.Error("startup failed: %v", err)
		return 1
	}

	if opts.once {
		if err := ag.syncOnce(ctx); err != nil {
			logger.Error("sync run failed: %v", err)
			return 1
		}
		return 0
	}

	logger.Info("loop mode: interval=%s", opts.interval)
	timer := time.NewTimer(opts.interval)
	defer timer.Stop()
	for {
		if err := ag.syncOnce(ctx); err != nil {
			// Keep looping: a transient failure (server down, Wi-Fi
			// dropped) should not kill the on-device agent.
			logger.Error("sync run failed (will retry in %s): %v", opts.interval, err)
		}
		timer.Reset(opts.interval)
		select {
		case <-ctx.Done():
			logger.Info("stopping (signal received)")
			return 0
		case <-timer.C:
		}
	}
}

func parseFlags(args []string, stderr io.Writer) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("readeckobo-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.configPath, "config", defaultConfigPath,
		"path to the KEY=VALUE config file; sidecars (index.json, ledger.json, agent.log, agent.lock) live next to it")
	fs.BoolVar(&opts.once, "once", false, "run a single sync pass and exit")
	fs.DurationVar(&opts.interval, "interval", defaultInterval, "loop mode: delay between sync passes")
	fs.StringVar(&opts.koboDB, "kobo-db", defaultKoboDBPath, "path to KoboReader.sqlite (override for tests)")
	fs.StringVar(&opts.onboard, "onboard", defaultOnboard, "onboard storage root (override for tests)")
	fs.BoolVar(&opts.noRescan, "no-rescan", false, "dry run: download/remove files but never invoke the NickelDBus rescan")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: readeckobo-agent [flags]\n\n")
		fmt.Fprintf(stderr, "On-device Readeck sync agent.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if opts.interval <= 0 {
		return opts, fmt.Errorf("--interval must be positive")
	}
	return opts, nil
}
