package replication

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func nativeRangeFixture(t *testing.T) (*NativeSource, *OrderingJournal, [][]byte, []LocalCommit) {
	t.Helper()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x31}, 32)
	options := NativeSourceOptions{AuditPath: filepath.Join(root, "audit.log"), GovernancePath: filepath.Join(root, "governance.log"), LedgerPath: filepath.Join(root, "ledger.log"), MetadataPath: filepath.Join(root, "metadata.log"), LedgerKey: key, MetadataKey: key, AuditKey: key, GovernanceKey: key}
	var auditFrames, governanceFrames, metadataFrames, ledgerFrames [][]byte
	ledgerLog, err := ledger.OpenWithOptions(options.LedgerPath, ledger.NewStatus(), ledger.Options{MaxBatch: 1, ChainKey: key,
		AfterDurable: func(batch ledger.DurableBatch) (uint64, error) {
			ledgerFrames = append(ledgerFrames, append([]byte(nil), batch.Frames...))
			return uint64(len(ledgerFrames)), nil
		}, WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.OpenWithOptions(options.AuditPath, key, audit.Options{AfterDurable: func(batch audit.DurableBatch) error {
		auditFrames = append(auditFrames, append([]byte(nil), batch.Frames...))
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	governanceLog, err := governance.OpenWithOptions(options.GovernancePath, key, governance.Options{AfterDurable: func(batch governance.DurableBatch) error {
		governanceFrames = append(governanceFrames, append([]byte(nil), batch.Frames...))
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	metadataLog, err := metadatajournal.StartEpoch(options.MetadataPath, key, metadatajournal.EpochHeader{Epoch: 1, Reason: "native-range-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := metadataLog.SetAfterDurable(func(batch metadatajournal.DurableBatch) (uint64, error) {
		metadataFrames = append(metadataFrames, append([]byte(nil), batch.Frames...))
		return uint64(len(metadataFrames)), nil
	}, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := ledgerLog.Append(context.Background(), ledger.Event{EventID: fmt.Sprintf("ledger_%d", i), Kind: ledger.EventReservationCreated, RequestID: "req_1", AttemptID: fmt.Sprintf("att_%d", i), ProjectID: "prj_1", PeriodID: "prj_1:2026-10-02:tz1", OccurredAt: time.Unix(123, 0).UTC(), ReservationMicrosUSD: ledger.MicrosUSD(100), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := auditLog.Append(context.Background(), audit.Event{EventID: fmt.Sprintf("audit_%d", i), OccurredAt: time.Unix(123, 0).UTC(), ActorType: "system", Action: "source.range", Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
		if _, err := governanceLog.Append(context.Background(), governance.Event{EventID: fmt.Sprintf("gov_%d", i), ProjectID: "prj_1", WorkUnitID: "wku_1", DefinitionID: "odef_1", DefinitionVersion: 1, OutcomeID: fmt.Sprintf("out_%d", i), Value: "accepted", ReporterKeyID: "key_1", ObservedAt: time.Unix(123, 0).UTC(), IngestedAt: time.Unix(123, 0).UTC(), Revision: 1, IdempotencyKeyHash: "sha256:" + strings.Repeat("a", 64), RequestFingerprint: "sha256:" + strings.Repeat("b", 64)}); err != nil {
			t.Fatal(err)
		}
		if _, err := metadataLog.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte(fmt.Sprintf("p_%d", i)), Value: []byte("value")}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := auditLog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := governanceLog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := metadataLog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ledgerLog.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	primary, journal := newTestPrimary(t, &recordingOutbound{})
	t.Cleanup(func() { _ = journal.Close() })
	anchor, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished})
	if err != nil {
		t.Fatal(err)
	}
	commits := []LocalCommit{anchor}
	for i := uint64(1); i <= 2; i++ {
		for _, data := range []struct {
			store Store
			bytes []byte
		}{{StoreAudit, auditFrames[i-1]}, {StoreGovernance, governanceFrames[i-1]}, {StoreMetadata, metadataFrames[i-1]}, {StoreLedger, ledgerFrames[i-1]}} {
			commit, err := primary.RecordDurable(Frame{Kind: KindData, Store: data.store, StoreGeneration: 1, StoreSequenceFirst: i, StoreSequenceLast: i, Payload: data.bytes})
			if err != nil {
				t.Fatal(err)
			}
			commits = append(commits, commit)
		}
	}
	return source, journal, auditFrames, commits
}

func TestNativeRangeReadersPreserveExactOrderingFramesAndDigestAuthority(t *testing.T) {
	source, journal, _, commits := nativeRangeFixture(t)
	var recovered []LocalCommit
	if err := ReconstructRangeEach("production-a", "inc_01", 1, uint64(len(commits)), journal, source, func(commit LocalCommit) error {
		recovered = append(recovered, commit)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(recovered) != len(commits) {
		t.Fatal(len(recovered))
	}
	for i := range commits {
		if recovered[i].Index != commits[i].Index || !bytes.Equal(recovered[i].Encoded, commits[i].Encoded) {
			t.Fatal("native cursor changed ordered wire frame", i)
		}
	}
	// A new native file can authenticate with the same key yet carry bytes the
	// ordering authority never approved. Native verification cannot authorize it.
	replacement := source.options.AuditPath + ".new"
	log, err := audit.Open(replacement, source.options.AuditKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), audit.Event{EventID: "different", OccurredAt: time.Unix(123, 0).UTC(), ActorType: "system", Action: "different.valid.history", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source.options.AuditPath); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(StoreAudit, 1, 1, 1); err != nil {
		t.Fatal("replacement should pass its native authentication", err)
	}
	consumed := 0
	if err := ReconstructRangeEach("production-a", "inc_01", 2, 2, journal, source, func(LocalCommit) error { consumed++; return nil }); err == nil || consumed != 0 {
		t.Fatal("native authentication bypassed ordering digest", err, consumed)
	}
}

func TestNativeMetadataRangeReaderPreservesOrderingAuthorityAndEpoch(t *testing.T) {
	source, journal, _, _ := nativeRangeFixture(t)
	if _, err := source.Read(StoreMetadata, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	log, err := metadatajournal.StartEpoch(source.options.MetadataPath, source.options.MetadataKey, metadatajournal.EpochHeader{Epoch: 1, Reason: "different-authenticated-history"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("different"), Value: []byte("value")}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(StoreMetadata, 1, 1, 1); err != nil {
		t.Fatal("replacement should authenticate natively", err)
	}
	consumed := 0
	if err := ReconstructRangeEach("production-a", "inc_01", 4, 4, journal, source, func(LocalCommit) error { consumed++; return nil }); err == nil || consumed != 0 {
		t.Fatal("native metadata bypassed ordering digest", err, consumed)
	}
	if _, err := source.Read(StoreMetadata, 2, 1, 1); err == nil {
		t.Fatal("wrong metadata epoch accepted")
	}
}

func TestNativeMetadataTailMutationInvalidatesRangeAuthority(t *testing.T) {
	for _, stage := range []string{"truncate", "repair", "failed-truncate"} {
		t.Run(stage, func(t *testing.T) {
			source, _, _, _ := nativeRangeFixture(t)
			_, err := source.Read(StoreMetadata, 1, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			second, err := metadatajournal.ReadReplicationFrames(source.options.MetadataPath, source.options.MetadataKey, 1, 2, 2)
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(source.options.MetadataPath)
			if err != nil {
				t.Fatal(err)
			}
			contents[len(contents)-len(second)-1] ^= 1
			if stage == "failed-truncate" {
				if err := os.WriteFile(source.options.MetadataPath, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "repair" {
				err = source.RepairReplicaTail(StoreMetadata, StoreCursor{Generation: 1, Sequence: 2})
			} else {
				err = source.Truncate(StoreMetadata, StoreCursor{Generation: 1, Sequence: 1})
			}
			if (err != nil) != (stage == "failed-truncate") {
				t.Fatal(err)
			}
			if err := os.WriteFile(source.options.MetadataPath, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := source.Read(StoreMetadata, 1, 2, 2); err == nil {
				t.Fatal("metadata tail mutation retained old prefix authority")
			}
		})
	}
}

func TestNativeSourceTailMutationAlwaysInvalidatesRangeAuthority(t *testing.T) {
	for _, stage := range []string{"truncate", "repair", "failed-truncate"} {
		t.Run(stage, func(t *testing.T) {
			source, _, frames, _ := nativeRangeFixture(t)
			if _, err := source.Read(StoreAudit, 1, 1, 1); err != nil {
				t.Fatal(err)
			}
			bad := bytes.Join(frames, nil)
			bad[len(frames[0])-1] ^= 1
			if stage == "failed-truncate" {
				if err := os.WriteFile(source.options.AuditPath, bad, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if stage == "repair" {
				err = source.RepairReplicaTail(StoreAudit, StoreCursor{Generation: 1, Sequence: 2})
			} else {
				err = source.Truncate(StoreAudit, StoreCursor{Generation: 1, Sequence: 1})
			}
			if (err != nil) != (stage == "failed-truncate") {
				t.Fatal(err)
			}
			if err := os.WriteFile(source.options.AuditPath, bad, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := source.Read(StoreAudit, 1, 2, 2); err == nil {
				t.Fatal("tail mutation retained old prefix authority")
			}
		})
	}
}

func TestNativeLedgerRangeReaderPreservesOrderingAuthority(t *testing.T) {
	source, journal, _, _ := nativeRangeFixture(t)
	if _, err := source.Read(StoreLedger, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	replacement := source.options.LedgerPath + ".new"
	log, err := ledger.OpenWithOptions(replacement, ledger.NewStatus(), ledger.Options{ChainKey: source.options.LedgerKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), ledger.Event{EventID: "different", Kind: ledger.EventReservationCreated, RequestID: "req_1", AttemptID: "att_other", ProjectID: "prj_1", PeriodID: "prj_1:2026-10-02:tz1", OccurredAt: time.Unix(123, 0).UTC(), ReservationMicrosUSD: ledger.MicrosUSD(900), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source.options.LedgerPath); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(StoreLedger, 1, 1, 1); err != nil {
		t.Fatal("valid replacement failed native authentication", err)
	}
	consumed := 0
	if err := ReconstructRangeEach("production-a", "inc_01", 5, 5, journal, source, func(LocalCommit) error { consumed++; return nil }); err == nil || consumed != 0 {
		t.Fatal("Ledger native authentication bypassed ordering digest", err, consumed)
	}
}

func TestNativeLedgerTailMutationAlwaysInvalidatesRangeAuthority(t *testing.T) {
	for _, stage := range []string{"truncate", "repair", "failed-truncate", "repair-roll"} {
		t.Run(stage, func(t *testing.T) {
			source, _, _, _ := nativeRangeFixture(t)
			if _, err := source.Read(StoreLedger, 1, 1, 1); err != nil {
				t.Fatal(err)
			}
			second, err := ledger.ReadReplicationFrames(source.options.LedgerPath, source.options.LedgerKey, 1, 2, 2)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(source.options.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			raw[len(raw)-len(second)-1] ^= 1
			if stage == "failed-truncate" {
				if err := os.WriteFile(source.options.LedgerPath, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch stage {
			case "repair":
				err = source.RepairReplicaTail(StoreLedger, StoreCursor{Generation: 1, Sequence: 2})
			case "repair-roll":
				err = source.RepairReplicaLedgerRoll(StoreCursor{Generation: 1, Sequence: 2})
			default:
				err = source.Truncate(StoreLedger, StoreCursor{Generation: 1, Sequence: 1})
			}
			if (err != nil) != (stage == "failed-truncate") {
				t.Fatal(err)
			}
			if err := os.WriteFile(source.options.LedgerPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := source.Read(StoreLedger, 1, 2, 2); err == nil {
				t.Fatal("Ledger tail mutation retained old prefix authority")
			}
		})
	}
}
