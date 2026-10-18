package app

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

// A config copied from the v0.8.5 example keeps `enabled: false` through
// migration, so the operator has to be told once the active WAL is already past
// the threshold sealing would have held it to — and not before, and not at all
// when sealing is on.
func TestWarnAboutUnsealedLedger(t *testing.T) {
	warned := func(enabled bool, activeBytes int64) bool {
		var buffer strings.Builder
		runtime := &Runtime{
			logger: slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})),
			config: config.Config{Ledger: config.Ledger{Seal: config.LedgerSeal{Enabled: enabled}}},
		}
		runtime.warnAboutUnsealedLedger(activeBytes)
		return strings.Contains(buffer.String(), "ledger sealing is off")
	}
	past := config.DefaultLedgerSealMaxActiveBytes + 1
	if !warned(false, past) {
		t.Fatal("sealing off with the active WAL past the default threshold was not reported")
	}
	if warned(false, config.DefaultLedgerSealMaxActiveBytes) {
		t.Fatal("warned before the active WAL passed the default threshold")
	}
	if warned(true, past) {
		t.Fatal("warned although sealing is on")
	}
}
