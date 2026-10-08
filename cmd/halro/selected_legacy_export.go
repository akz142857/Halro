package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
)

func runSelectedLegacyExportCommand(arguments []string, out io.Writer) error {
	f := flag.NewFlagSet("metadata export-legacy", flag.ContinueOnError)
	configPath := f.String("config", "config.yaml", "stopped selected Standalone configuration")
	output := f.String("output", "", "absent independent private output directory")
	if err := f.Parse(arguments); err != nil {
		return err
	}
	if f.NArg() != 0 || *output == "" {
		return errors.New("usage: halro metadata export-legacy --config <path> --output <absent independent directory>")
	}
	cfg, err := config.Load(*configPath, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	result, err := app.ExportSelectedLegacy(ctx, cfg, *configPath, *output)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
