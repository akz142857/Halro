package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
	bbolt "go.etcd.io/bbolt"
)

func TestVerifyUsageDiskReferenceSupportsBothCatalogsAndFormats(t *testing.T) {
	for _, format := range []string{usage.FormatParquet, usage.FormatNDJSON} {
		for _, paged := range []bool{false, true} {
			t.Run(format+map[bool]string{false: "/legacy", true: "/paged"}[paged], func(t *testing.T) {
				r, _ := summaryRuntime(t, 2)
				r.stopBackgroundWork()
				cfg := r.config
				cfg.Usage.ExportFormat = format
				if paged {
					installDiskUsageQueries(t, r)
					r.usageExporter, _ = usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: format})
					r.exportUsageParquet()
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if !paged {
					if _, err := exportLegacyUsageFixture(cfg); err != nil {
						t.Fatal(err)
					}
				}
				scratch := t.TempDir()
				t.Setenv("TMPDIR", scratch)
				before := doctorTree(t, cfg.Storage.DataDir)
				report, err := VerifyUsage(context.Background(), cfg)
				if err != nil || report.LedgerRecords != 2 || report.ParquetRecords != 2 || report.Role != "standalone" || report.ThroughSequence == 0 || report.SourceHead.Sequence == 0 || report.NativeHead != report.AllowedLedgerHead {
					t.Fatal(report, err)
				}
				if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
					t.Fatal("verification changed source or existing derivatives")
				}
				entries, err := os.ReadDir(scratch)
				if err != nil || len(entries) != 0 {
					t.Fatal("fresh reference survived verification", entries, err)
				}
			})
		}
	}
}

func TestVerifyUsageDiskReferenceRequiresExplicitRetainedInterval(t *testing.T) {
	r, _ := summaryRuntime(t, 4)
	r.stopBackgroundWork()
	cfg := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := exportLegacyUsageFixture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a suffix within the actual old partition, then publish an old JSON
	// manifest with explicit native frontier. Native history is unchanged.
	aggregate, exporter, closeResources, err := openUsageOffline(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := aggregate.Snapshot()
	t.Cleanup(func() { _ = closeResources() })
	if len(snapshot.Attempts) != 4 {
		t.Fatal(len(snapshot.Attempts))
	}
	after := snapshot.Attempts[1].Sequence
	for _, entry := range manifest.Files {
		if err := os.Remove(filepath.Join(cfg.UsagePath(), entry.Path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(cfg.UsagePath(), "manifest.json")); err != nil {
		t.Fatal(err)
	}
	snapshot.Attempts = snapshot.Attempts[2:]
	suffix, err := exporter.Export(snapshot)
	if err != nil || len(suffix.Files) == 0 {
		t.Fatal(suffix, err)
	}
	// The offline Exporter owns the directory lifetime. Keep that ownership
	// through this fixture rewrite, then release it before independent Verify.
	if err := closeResources(); err != nil {
		t.Fatal(err)
	}
	before := doctorTree(t, cfg.Storage.DataDir)
	if report, err := VerifyUsage(context.Background(), cfg); err == nil || report != (usage.ReconciliationReport{}) {
		t.Fatal("missing prefix inferred a floor", report, err)
	}
	report, err := VerifyUsageAfter(context.Background(), cfg, after)
	if err != nil || report.LedgerRecords != 2 || report.AfterSequence != after {
		t.Fatal(report, err)
	}
	if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
		t.Fatal("explicit range altered archive")
	}
}

func offlineUsageMemberFixture(t *testing.T) (config.Config, replication.MemberState, [32]byte, []byte, ledger.Watermark) {
	t.Helper()
	r, _ := summaryRuntime(t, 2)
	r.stopBackgroundWork()
	r.exportUsageParquet()
	applied, _, _ := r.ledger.ChainHead()
	billOneRequest(t, r, "excluded_durable_usage")
	cfg := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	master, err := unlockMasterKey(context.Background(), cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	v, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	key, err := loadLedgerHMACKey(store, v, master)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(key) })
	metadataKey, err := loadMetadataJournalHMACKey(store, v, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(metadataKey)
	metadata, err := store.ReadMetadataAppliedPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metadataHead, err := metadatajournal.ReplicationHeadAt(store.MetadataJournalPath(), metadataKey, metadata.Epoch, metadata.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{ClusterID: "usage-offline", NodeID: "node-0", Peers: []config.ReplicationPeer{{Name: "node-1", Address: "127.0.0.1:9311", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	clusterKey, err := replication.DeriveClusterKey(master, "inc_usage")
	if err != nil {
		t.Fatal(err)
	}
	header := replication.OrderingHeader{ClusterID: cfg.Replication.ClusterID, Incarnation: "inc_usage"}
	header.StoreCursors[replication.StoreLedger-replication.StoreLedger] = replication.StoreCursor{Generation: 1}
	header.StoreCursors[replication.StoreMetadata-replication.StoreLedger] = replication.StoreCursor{Generation: metadata.Epoch, Sequence: metadata.Sequence}
	header.StoreHeads[replication.StoreMetadata-replication.StoreLedger] = metadataHead
	journal, err := replication.OpenOrderingJournal(cfg.OrderingJournalPath(), clusterKey[:], header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	reader, err := ledger.NewReplicationRangeReader(cfg.LedgerPath(), key, replication.MaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, native, err := ledger.InspectProjectionCursor(context.Background(), cfg.LedgerPath(), key, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for index := uint64(1); index <= 3; index++ {
		frame := replication.Frame{ClusterID: header.ClusterID, Incarnation: header.Incarnation, Index: index, Term: 7, Kind: replication.KindLeadershipEstablished}
		if index > 1 {
			frame.Kind, frame.Store, frame.StoreGeneration = replication.KindData, replication.StoreLedger, 1
			frame.StoreSequenceFirst, frame.StoreSequenceLast = 1, applied.Sequence
			if index == 3 {
				frame.StoreSequenceFirst, frame.StoreSequenceLast = applied.Sequence+1, native.Head.Sequence
			}
			frame.Payload, err = reader.Read(1, frame.StoreSequenceFirst, frame.StoreSequenceLast)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			t.Fatal(err)
		}
		_, err = journal.Append(replication.OrderingRecord{Kind: frame.Kind, Store: frame.Store, StoreGeneration: frame.StoreGeneration, StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast, Index: index, Term: 7, FrameDigest: digest})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _, mac := journal.Head()
	member := replication.MemberState{Version: replication.StateVersion, ClusterID: header.ClusterID, Incarnation: header.Incarnation, NodeID: cfg.Replication.NodeID, Role: replication.RoleReplica, Term: 7, PromisedTerm: 7, DurableIndex: 3, ConfirmedIndex: 3, AppliedIndex: 2, OrderingHeadMAC: mac, Projection: replication.ProjectionState{Index: 2, MetadataEpoch: metadata.Epoch, MetadataSequence: metadata.Sequence}, Peers: []replication.StatePeer{{Name: "node-1", Address: "127.0.0.1:9311", SPKISHA256: cfg.Replication.Peers[0].SPKISHA256}}}
	if err := replication.WriteState(cfg.ReplicationStatePath(), member, clusterKey[:]); err != nil {
		t.Fatal(err)
	}
	return cfg, member, clusterKey, key, applied
}

func TestVerifyUsageDiskReferenceHonorsReplicaAppliedAndPrimaryDurableCuts(t *testing.T) {
	cfg, member, key, _, applied := offlineUsageMemberFixture(t)
	before := doctorTree(t, cfg.Storage.DataDir)
	report, err := VerifyUsage(context.Background(), cfg)
	if err != nil || report.Role != "replica" || report.OrderingIndex != 2 || report.AllowedLedgerHead != applied || report.NativeHead.Sequence <= report.AllowedLedgerHead.Sequence || report.LedgerRecords != 2 || report.SourceHead.Sequence != applied.Sequence {
		t.Fatal(report, err)
	}
	if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
		t.Fatal("Replica verification wrote member/native/derivatives")
	}
	member.Role, member.ConfirmedIndex = replication.RolePrimary, 2
	if err := replication.WriteState(cfg.ReplicationStatePath(), member, key[:]); err != nil {
		t.Fatal(err)
	}
	report, err = VerifyUsage(context.Background(), cfg)
	if err != nil || report.Role != "primary" || report.OrderingIndex != 3 || report.AllowedLedgerHead != report.NativeHead || report.SourceHead != report.NativeHead || report.LedgerRecords != 2 {
		t.Fatal(report, err)
	}
}

func TestVerifyUsageDiskReferenceRejectsUntrustedMemberAndExcludedTail(t *testing.T) {
	for _, scenario := range []string{"metadata-cut", "awaiting", "member-term", "member-mac", "ordering-tail", "partial-ordering", "baseline-head", "digest", "reseed"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, member, key, _, _ := offlineUsageMemberFixture(t)
			switch scenario {
			case "metadata-cut":
				member.Projection.MetadataSequence++
				if err := replication.WriteState(cfg.ReplicationStatePath(), member, key[:]); err != nil {
					t.Fatal(err)
				}
			case "awaiting":
				member.Role = replication.RoleAwaitingDecision
				if err := replication.WriteState(cfg.ReplicationStatePath(), member, key[:]); err != nil {
					t.Fatal(err)
				}
			case "member-term":
				member.Term, member.PromisedTerm = 8, 8
				if err := replication.WriteState(cfg.ReplicationStatePath(), member, key[:]); err != nil {
					t.Fatal(err)
				}
			case "member-mac":
				raw, _ := os.ReadFile(cfg.ReplicationStatePath())
				var obj map[string]any
				if err := json.Unmarshal(raw, &obj); err != nil {
					t.Fatal(err)
				}
				obj["mac"] = strings.Repeat("0", 64)
				raw, _ = json.Marshal(obj)
				if err := os.WriteFile(cfg.ReplicationStatePath(), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			case "ordering-tail", "partial-ordering":
				raw, _ := os.ReadFile(cfg.OrderingJournalPath())
				if scenario == "ordering-tail" {
					raw[len(raw)-1] ^= 1
				} else {
					raw = append(raw, 1, 2)
				}
				if err := os.WriteFile(cfg.OrderingJournalPath(), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			case "baseline-head", "digest":
				// A fully authenticated alternative ordering chain at the same
				// cursors must still match native identity/digests.
				header, err := replication.ReadOrderingHeader(cfg.OrderingJournalPath(), key[:])
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "baseline-head" {
					header.StoreCursors[0] = replication.StoreCursor{Generation: 1, Sequence: 1}
					header.StoreHeads[0][0] = 0x55
				}
				if err := os.Remove(cfg.OrderingJournalPath()); err != nil {
					t.Fatal(err)
				}
				j, err := replication.OpenOrderingJournal(cfg.OrderingJournalPath(), key[:], header, 0, [32]byte{})
				if err != nil {
					t.Fatal(err)
				}
				var digest [32]byte
				digest[0] = 0x77
				if _, err := j.Append(replication.OrderingRecord{Kind: replication.KindLeadershipEstablished, Index: 1, Term: 7, FrameDigest: digest}); err != nil {
					t.Fatal(err)
				}
				if scenario == "digest" {
					if _, err := j.Append(replication.OrderingRecord{Kind: replication.KindData, Store: replication.StoreLedger, Index: 2, Term: 7, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, FrameDigest: digest}); err != nil {
						t.Fatal(err)
					}
				}
				member.DurableIndex, _, member.OrderingHeadMAC = j.Head()
				member.AppliedIndex, member.ConfirmedIndex, member.Projection.Index = 1, member.DurableIndex, 1
				if err := j.Close(); err != nil {
					t.Fatal(err)
				}
				if err := replication.WriteState(cfg.ReplicationStatePath(), member, key[:]); err != nil {
					t.Fatal(err)
				}
			case "reseed":
				if err := os.WriteFile(filepath.Join(cfg.ClusterDirectoryPath(), reseedRequiredMarker), []byte("requires reseed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := doctorTree(t, cfg.Storage.DataDir)
			if report, err := VerifyUsage(context.Background(), cfg); err == nil || report != (usage.ReconciliationReport{}) {
				t.Fatal("untrusted HA evidence returned report", report, err)
			}
			if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
				t.Fatal("authentication refusal changed files")
			}
		})
	}
}

func TestVerifyUsageDiskReferenceCancellationDoesNotCreateAnything(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := VerifyUsage(ctx, cfg); !errors.Is(err, context.Canceled) || report != (usage.ReconciliationReport{}) {
		t.Fatal(report, err)
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled verification created data", err)
	}
}

func TestVerifyUsageDiskReferenceFinalEvidenceAndCleanup(t *testing.T) {
	for _, scenario := range []string{"member-cut", "source", "metadata-file", "cancel", "cleanup-identity"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, member, key, _, _ := offlineUsageMemberFixture(t)
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var displaced string
			report, err := verifyUsageAfter(ctx, cfg, 0, func() error {
				switch scenario {
				case "member-cut":
					member.AppliedIndex, member.Projection.Index = 3, 3
					return replication.WriteState(cfg.ReplicationStatePath(), member, key[:])
				case "source":
					file, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, err = file.Write([]byte("late torn bytes"))
					return errors.Join(err, file.Close())
				case "cancel":
					cancel()
				case "metadata-file":
					raw, err := os.ReadFile(cfg.MetadataPath())
					if err != nil {
						return err
					}
					if err := os.WriteFile(cfg.MetadataPath()+".next", raw, 0o600); err != nil {
						return err
					}
					return os.Rename(cfg.MetadataPath()+".next", cfg.MetadataPath())
				case "cleanup-identity":
					entries, err := os.ReadDir(scratch)
					if err != nil || len(entries) != 1 {
						t.Fatal(entries, err)
					}
					name := filepath.Join(scratch, entries[0].Name())
					displaced = name + ".displaced"
					if err := os.Rename(name, displaced); err != nil {
						return err
					}
					if err := os.Mkdir(name, 0o700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(name, "foreign-owner"), []byte("must survive"), 0o600)
				}
				return nil
			})
			if err == nil || report != (usage.ReconciliationReport{}) {
				t.Fatal("final evidence failure escaped as success", report, err)
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			entries, readErr := os.ReadDir(scratch)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "cleanup-identity" {
				if len(entries) != 2 || !strings.Contains(err.Error(), "scratch identity changed") {
					t.Fatal(entries, err)
				}
				for _, entry := range entries {
					if filepath.Join(scratch, entry.Name()) != displaced {
						data, err := os.ReadFile(filepath.Join(scratch, entry.Name(), "foreign-owner"))
						if err != nil || string(data) != "must survive" {
							t.Fatal("cleanup deleted foreign directory", string(data), err)
						}
					}
				}
			} else if len(entries) != 0 {
				t.Fatal("failed job retained reference DB", entries)
			}
		})
	}
}

func TestVerifyUsageDiskReferenceRefusesScratchUnderNativeDirectory(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	cfg := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := exportLegacyUsageFixture(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", cfg.Storage.DataDir)
	before := doctorTree(t, cfg.Storage.DataDir)
	if report, err := VerifyUsage(context.Background(), cfg); err == nil || report != (usage.ReconciliationReport{}) || !strings.Contains(err.Error(), "scratch parent must be outside") {
		t.Fatal(report, err)
	}
	if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
		t.Fatal("bad TMPDIR wrote native tree")
	}
}

func TestVerifyUsageDiskReferenceChecksHistoricalAnchorBehindNativeHead(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	r.stopBackgroundWork()
	head, hash, _ := r.ledger.ChainHead()
	billOneRequest(t, r, "later_native_anchor")
	cfg := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := exportLegacyUsageFixture(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(cfg.MetadataPath(), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash[0] ^= 1
	anchor, err := json.Marshal(boltstore.LedgerChainCheckpoint{Generation: head.Generation, Sequence: head.Sequence, Offset: head.Offset, Hash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("ledger_chain_checkpoint"), anchor)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := doctorTree(t, cfg.Storage.DataDir)
	if report, err := VerifyUsage(context.Background(), cfg); err == nil || report != (usage.ReconciliationReport{}) || !strings.Contains(err.Error(), "trusted checkpoint") {
		t.Fatal("older diverged anchor escaped", report, err)
	}
	if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
		t.Fatal("anchor mismatch changed data")
	}
}

func TestVerifyUsageDiskReferenceCannotDowngradeMemberOrCreateMemberState(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	cfg.Replication = nil
	before := doctorTree(t, cfg.Storage.DataDir)
	if report, err := VerifyUsage(context.Background(), cfg); err == nil || report != (usage.ReconciliationReport{}) || !strings.Contains(err.Error(), "cannot downgrade") {
		t.Fatal(report, err)
	}
	if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
		t.Fatal("missing replication config altered member")
	}
	cfg = testConfig(t)
	cfg.Replication = &config.Replication{}
	if report, err := VerifyUsage(context.Background(), cfg); err == nil || report != (usage.ReconciliationReport{}) || !strings.Contains(err.Error(), "join or re-seed") {
		t.Fatal(report, err)
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verification created member directory", err)
	}
}

// Small resident fixture only, to keep legacy JSON coverage independent of the
// actual disk compaction entry. Production compact never uses this helper.
func exportLegacyUsageFixture(cfg config.Config) (usage.Manifest, error) {
	a, e, closeResources, err := openUsageOffline(context.Background(), cfg)
	if err != nil {
		return usage.Manifest{}, err
	}
	defer closeResources()
	// This private fixture deliberately rebuilds legacy JSON, independent of
	// default Runtime's paged publications. Its exclusive directory lock remains
	// held while replacing only this test's disposable Usage archive.
	if err := e.Close(); err != nil {
		return usage.Manifest{}, err
	}
	if err := os.RemoveAll(cfg.UsagePath()); err != nil {
		return usage.Manifest{}, err
	}
	e, err = usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: cfg.Usage.ExportFormat})
	if err != nil {
		return usage.Manifest{}, err
	}
	defer e.Close()
	return e.Export(a.Snapshot())
}
