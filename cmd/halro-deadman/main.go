package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/deadman"
	"github.com/akz142857/Halro/internal/safelog"
)

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("halro-deadman", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/halro-deadman/config.yaml", "dead-man configuration file")
	check := flags.Bool("check-config", false, "validate configuration and exit")
	// The watchdog lives outside Halro's failure domain, which means it is also
	// outside its release evidence unless it can say what it is: an operator
	// looking at a probe that stopped reporting has to be able to tell which
	// build is running without unpacking the archive it came from.
	showVersion := flags.Bool("version", false, "print build identity and exit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	// On its own, -version answers without a config so a probe can say what it
	// is before anything is deployed. -check-config wins when both are given:
	// a deployment gate that composes its flag list must not be satisfied by a
	// run that validated nothing.
	if *showVersion && !*check {
		return json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
	}
	cfg, err := deadman.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *showVersion {
		return json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
	}
	if *check {
		return nil
	}
	// The dead-man holds bearer tokens for every target it probes, so its logs
	// need the same scrubbing the gateway's do.
	logger := safelog.New(slog.NewJSONHandler(os.Stderr, nil))
	engine, err := deadman.New(cfg, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return engine.Run(ctx)
}
