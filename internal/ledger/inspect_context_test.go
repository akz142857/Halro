package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectionCancellationAndManifestReplacementNeverPublishHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, NewStatus())
	for _, id := range []string{"one", "two"} {
		if _, err := log.Append(context.Background(), Event{Kind: EventRequestAccepted, EventID: id, RequestID: id, ProjectID: "p", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for _, authenticated := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		visited := 0
		consume := func(Record) error { visited++; cancel(); return nil }
		if authenticated {
			report, partial, err := InspectReplayAuthenticatedContext(ctx, path, testChainKey, consume)
			if !errors.Is(err, context.Canceled) || report != (ChainReport{}) || partial || visited != 1 {
				t.Fatal(report, partial, visited, err)
			}
		} else {
			head, partial, err := InspectReplayContext(ctx, path, consume)
			if !errors.Is(err, context.Canceled) || head != (Watermark{}) || partial || visited != 1 {
				t.Fatal(head, partial, visited, err)
			}
		}
		cancel()
	}
	manifest := filepath.Join(filepath.Dir(path), segmentManifestName)
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	head, partial, err := InspectReplayContext(context.Background(), path, func(Record) error {
		if err := os.Rename(manifest, manifest+".original"); err != nil {
			return err
		}
		return os.WriteFile(manifest, data, 0600)
	})
	if err == nil || head != (Watermark{}) || partial {
		t.Fatal("replaced manifest published a cut", head, partial, err)
	}
}
