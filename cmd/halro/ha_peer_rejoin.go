package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
)

func runHAPeerRejoin(arguments []string, logger *slog.Logger, out io.Writer) error {
	flags := flag.NewFlagSet("cluster ha-rejoin", flag.ContinueOnError)
	path := flags.String("config", "config.yaml", "path to YAML configuration")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("cluster ha-rejoin accepts only --config; candidate and term come from the original durable peer Promise")
	}
	cfg, err := config.Load(*path, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return executeHAPeerRejoin(ctx, cfg, logger, out)
}
func executeHAPeerRejoin(ctx context.Context, cfg config.Config, _ *slog.Logger, out io.Writer) error {
	m, err := app.RejoinHASelectedMember(ctx, cfg)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"cluster_id": m.ClusterID, "incarnation": m.Incarnation, "node_id": m.NodeID, "role": m.Role, "term": m.Term, "promised_term": m.PromisedTerm, "durable_index": m.DurableIndex, "confirmed_index": m.ConfirmedIndex, "applied_index": m.AppliedIndex})
}
