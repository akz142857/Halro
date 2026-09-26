package audit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditDurableHookCapturesOneFsyncedBatchAndPoisonsOnFailure(t *testing.T) {
	var captured DurableBatch
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "audit.log"), randomKey(t), Options{
		AfterDurable: func(batch DurableBatch) error {
			captured = batch
			return errors.New("ordering disk full")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events := []Event{validEvent(1, "one"), validEvent(2, "two")}
	if _, err := log.AppendBatch(context.Background(), events); err == nil || !strings.Contains(err.Error(), "ordering disk full") {
		t.Fatalf("append error=%v", err)
	}
	if captured.FirstSequence != 1 || captured.LastSequence != 2 || len(captured.Frames) == 0 {
		t.Fatalf("captured=%#v", captured)
	}
	if _, err := log.Append(context.Background(), validEvent(3, "three")); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("poison error=%v", err)
	}
}
