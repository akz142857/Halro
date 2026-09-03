package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticatedInspectionReplaysEveryGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 3)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 4, 2)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	var sequences []uint64
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(record Record) error {
		sequences = append(sequences, record.Sequence)
		return nil
	})
	if err != nil || partial {
		t.Fatalf("report=%+v partial=%v err=%v", report, partial, err)
	}
	if len(sequences) != 5 || report.Head.Generation != 3 || report.Head.Sequence != 5 ||
		report.SealedGenerations != 2 || report.SealedAuthenticated != 5 || report.Authenticated != 0 ||
		!report.ChainVerified || report.ChainSequence != 5 || report.ChainOffset != 0 {
		t.Fatalf("sequences=%v report=%+v", sequences, report)
	}
	_, _, err = InspectReplayAuthenticated(path, testChainKey, func(Record) error { return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback error lost: %v", err)
	}
}

func TestAuthenticatedInspectionRefusesTampering(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		name := "active"
		if sealed {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "ledger.wal")
			log := openChained(t, path, nil)
			appendReservations(t, log, 1, 2)
			target := path
			var generation uint64
			if sealed {
				result, err := log.Roll()
				if err != nil {
					t.Fatal(err)
				}
				target, generation = filepath.Join(directory, result.Sealed.File), result.Sealed.Generation
				appendReservations(t, log, 3, 1)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			tamperWithSealedFrame(t, target)
			if sealed {
				repairManifestChecksum(t, directory, generation, target)
			}
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			visits := 0
			_, _, err = InspectReplayAuthenticated(path, testChainKey, func(Record) error { visits++; return nil })
			if !errors.Is(err, ErrTampered) || visits != 0 {
				t.Fatalf("visits=%d err=%v", visits, err)
			}
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("inspection changed the WAL: %v", err)
			}
		})
	}
}

func TestAuthenticatedInspectionRequiresKeyAndPreservesPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 1)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for _, key := range [][]byte{nil, {1}, bytes.Repeat([]byte{0xff}, 32)} {
		if _, _, err := InspectReplayAuthenticated(path, key, nil); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("torn")...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || !partial || report.Authenticated != 1 {
		t.Fatalf("report=%+v partial=%v err=%v", report, partial, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("partial tail changed: %v", err)
	}
}

func TestAuthenticatedInspectionRejectsDowngradeAcrossGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 1)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	event := validReservation("downgraded", "attempt_2")
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encodeFrameVersion(frameVersionPeriod, 2, event.Kind, payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InspectReplayAuthenticated(path, testChainKey, nil); !errors.Is(err, ErrTampered) {
		t.Fatalf("downgrade accepted: %v", err)
	}
}
