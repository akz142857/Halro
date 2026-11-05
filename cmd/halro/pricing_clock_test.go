package main

import "testing"

func TestPricingClockRecoveryRequiresExplicitReview(t *testing.T) {
	for _, args := range [][]string{{}, {"--deployment", "dep", "--apply"}, {"--deployment", "dep", "--price-id", "price"}, {"--deployment", "dep", "--apply", "--revision", "1", "--reason", "restored_backup", "--price-id", "price", "--reviewed-at", "2026-11-04T12:00:00Z"}} {
		if err := runPricingClockRecovery(args); err == nil {
			t.Fatalf("unsafe arguments accepted: %v", args)
		}
	}
}
