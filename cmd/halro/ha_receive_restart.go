package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
	"io"
	"log/slog"
	"time"
)

func runHAReceiveRestart(arguments []string, _ *slog.Logger, out io.Writer) error {
	flags := flag.NewFlagSet("cluster ha-receive-restart", flag.ContinueOnError)
	path := flags.String("config", "config.yaml", "path to YAML configuration")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("cluster ha-receive-restart accepts only --config; it preserves incomplete receipts and cannot choose progress or a root")
	}
	cfg, err := config.Load(*path, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	member, err := app.RestartHASelectedReceiveReceipt(ctx, cfg)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"cluster_id": member.ClusterID, "incarnation": member.Incarnation, "node_id": member.NodeID, "role": member.Role, "term": member.Term, "durable_index": member.DurableIndex, "confirmed_index": member.ConfirmedIndex, "applied_index": member.AppliedIndex})
}
