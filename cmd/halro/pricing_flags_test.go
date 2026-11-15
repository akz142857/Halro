package main

import (
	"log/slog"
	"strings"
	"testing"
)

func TestBootstrapRejectsExplicitNegativeCacheWritePrice(t *testing.T) {
	for _, name := range []string{"cache-write-micros-per-million", "cache-write-long-micros-per-million"} {
		t.Run(name, func(t *testing.T) {
			err := run([]string{"bootstrap", "--" + name, "-1"}, slog.Default())
			if err == nil || !strings.Contains(err.Error(), "cache write prices cannot be negative") {
				t.Fatalf("negative cache write price: %v", err)
			}
		})
	}
}
