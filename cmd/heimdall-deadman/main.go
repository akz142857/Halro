package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/akz142857/Heimdall/internal/deadman"
	"github.com/akz142857/Heimdall/internal/safelog"
)

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("heimdall-deadman", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/heimdall-deadman/config.yaml", "dead-man configuration file")
	check := flags.Bool("check-config", false, "validate configuration and exit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cfg, err := deadman.LoadConfig(*configPath)
	if err != nil {
		return err
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
