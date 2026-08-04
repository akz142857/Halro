package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
)

func TestLastKMSRecoveryUseIsBoundToCurrentSlotAndContract(t *testing.T) {
	key := make([]byte, 32)
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	base := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	events := []audit.Event{
		{EventID: "aud_wrong_slot", OccurredAt: base.Add(5 * time.Hour), ActorType: "local_cli", Action: "security.master_key.recovery_used", TargetType: "master_key_slot", TargetID: "old-recovery", Outcome: "success", ReasonCode: "break_glass_recovery"},
		{EventID: "aud_wrong_target", OccurredAt: base.Add(4 * time.Hour), ActorType: "local_cli", Action: "security.master_key.recovery_used", TargetType: "gateway", TargetID: "current-recovery", Outcome: "success", ReasonCode: "break_glass_recovery"},
		{EventID: "aud_wrong_reason", OccurredAt: base.Add(3 * time.Hour), ActorType: "local_cli", Action: "security.master_key.recovery_used", TargetType: "master_key_slot", TargetID: "current-recovery", Outcome: "success", ReasonCode: "untrusted"},
		{EventID: "aud_failed", OccurredAt: base.Add(2 * time.Hour), ActorType: "local_cli", Action: "security.master_key.recovery_used", TargetType: "master_key_slot", TargetID: "current-recovery", Outcome: "error", ReasonCode: "break_glass_recovery"},
		{EventID: "aud_valid_verify", OccurredAt: base, ActorType: "local_cli", Action: "security.master_key.recovery_used", TargetType: "master_key_slot", TargetID: "current-recovery", Outcome: "success", ReasonCode: "break_glass_recovery"},
		{EventID: "aud_valid_restore", OccurredAt: base.Add(time.Hour), ActorType: "local_cli", Action: "security.master_key.recovery_used", TargetType: "master_key_slot", TargetID: "current-recovery", Outcome: "success", ReasonCode: "break_glass_restore"},
	}
	for _, event := range events {
		if _, err := log.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := lastKMSRecoveryUse(log, "current-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if !latest.Equal(base.Add(time.Hour)) {
		t.Fatalf("latest=%v want=%v", latest, base.Add(time.Hour))
	}
	missing, err := lastKMSRecoveryUse(log, "")
	if err != nil || !missing.IsZero() {
		t.Fatalf("empty configured slot must not inherit audit evidence: latest=%v err=%v", missing, err)
	}
}
