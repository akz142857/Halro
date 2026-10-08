package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestHAReceiveRestartCLIRejectsCallerProgressAndDispatches(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{"--index", "12"}, {"--role", "replica"}, {"--term", "7"}, {"--drop-received"}, {"replica"}} {
		var out bytes.Buffer
		if err := runHAReceiveRestart(args, logger, &out); err == nil || out.Len() != 0 {
			t.Fatal("caller chose authority", args, err, out.String())
		}
	}
	if err := run([]string{"cluster", "ha-receive-restart", "--index", "12"}, logger); err == nil || strings.Contains(err.Error(), "unknown command") {
		t.Fatal("production dispatch missing", err)
	}
}
