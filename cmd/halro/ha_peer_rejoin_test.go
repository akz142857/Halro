package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

func TestHAPeerRejoinCLIRejectsCallerRoleTermOrCandidate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{"--candidate", "caller-choice"}, {"--term", "8"}, {"--no-peer-promise"}, {"primary"}} {
		var out bytes.Buffer
		if err := runHAPeerRejoin(args, logger, &out); err == nil || out.Len() != 0 {
			t.Fatal("caller supplied promotion authority", args, err, out.String())
		}
	}
	if err := run([]string{"cluster", "ha-rejoin", "--candidate", "caller-choice"}, logger); err == nil || strings.Contains(err.Error(), "unknown command") {
		t.Fatal("production dispatch missing", err)
	}
}
func TestHAPeerRejoinCLIRefusesStandaloneWithoutOutput(t *testing.T) {
	var out bytes.Buffer
	if err := executeHAPeerRejoin(context.Background(), config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), &out); err == nil || out.Len() != 0 {
		t.Fatal("Standalone acquired peer transition", err, out.String())
	}
}
