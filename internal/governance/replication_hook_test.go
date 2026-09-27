package governance

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func replicationHookEvent(id string) Event {
	return Event{
		EventID: id, ProjectID: "prj_1", WorkUnitID: "wku_1", DefinitionID: "odef_1", DefinitionVersion: 1,
		OutcomeID: "out_1", Value: "accepted", ReporterKeyID: "key_1", ObservedAt: time.Now().UTC(), IngestedAt: time.Now().UTC(),
		Revision: 1, IdempotencyKeyHash: shaLabel("one"), RequestFingerprint: shaLabel("two"),
	}
}

func TestGovernanceDurableHookCapturesOneFsyncedBatchAndPoisonsOnFailure(t *testing.T) {
	var captured DurableBatch
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "governance.log"), []byte("0123456789abcdef0123456789abcdef"), Options{
		AfterDurable: func(batch DurableBatch) error {
			captured = batch
			return errors.New("ordering disk full")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.AppendBatch(context.Background(), []Event{replicationHookEvent("gov_1"), replicationHookEvent("gov_2")}); err == nil || !strings.Contains(err.Error(), "ordering disk full") {
		t.Fatalf("append error=%v", err)
	}
	if captured.FirstSequence != 1 || captured.LastSequence != 2 || len(captured.Frames) == 0 {
		t.Fatalf("captured=%#v", captured)
	}
	if _, err := log.Append(context.Background(), replicationHookEvent("gov_3")); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("poison error=%v", err)
	}
}
