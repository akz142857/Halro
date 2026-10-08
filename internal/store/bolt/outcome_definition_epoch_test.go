package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

func legacyDefinitionTailFixture(t *testing.T) (string, []domain.OutcomeDefinition, metadatajournal.Head) {
	t.Helper()
	s, project, rows := definitionExportFixture(t, 2, 1)
	path := s.db.Path()
	// A schema-40 projection with a durable, unapplied legacy definition frame.
	// No new export-index operations exist in that frame.
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketOutcomeDefinitionExport, bucketOutcomeDefinitionExportMeta, bucketDeploymentPriceRevision} {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
		}
		if err := tx.Bucket(bucketMigrationHistory).Delete(versionKey(41)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMigrationHistory).Delete(versionKey(42)); err != nil {
			return err
		}
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(40))
	}); err != nil {
		t.Fatal(err)
	}
	row := domain.OutcomeDefinition{ID: "odef_0000", ProjectID: project.ID, Name: "durable", Version: 1, Revision: 1, DataType: domain.OutcomeBoolean,
		SuccessValues: []string{"true"}, CreatedAt: time.Now().UTC(), CreatedBy: "durable-only"}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{string(bucketOutcomeDefinitions)}, Key: outcomeDefinitionKey(row.ID, 1), Value: raw}}); err != nil {
		t.Fatal(err)
	}
	head := s.journal.Head()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path, append(rows, row), head
}

func assertDefinitionExportRows(t *testing.T, s *Store, expected []domain.OutcomeDefinition) {
	t.Helper()
	err := s.WithOutcomeDefinitionExport(context.Background(), func(v *OutcomeDefinitionExportView) error {
		actual, err := readAllDefinitionExport(context.Background(), v)
		if err != nil {
			return err
		}
		sort.Slice(actual, func(i, j int) bool { return actual[i].ID < actual[j].ID })
		want := append([]domain.OutcomeDefinition(nil), expected...)
		sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("export rows differ actual=%d expected=%d", len(actual), len(want))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOutcomeDefinitionSchema41RecoversLegacyDurableTailBeforePrefixPublication(t *testing.T) {
	for _, window := range []string{"normal", "after-schema-commit", "after-tail-recovery", "after-prefix-prepare", "after-epoch-file", "bad-key", "bad-native", "prepared-mismatch"} {
		t.Run(window, func(t *testing.T) {
			path, expected, oldHead := legacyDefinitionTailFixture(t)
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			if version, err := s.SchemaVersion(); err != nil || version != schemaVersion {
				t.Fatalf("schema=%d err=%v", version, err)
			}
			if err := s.WithOutcomeDefinitionExport(context.Background(), func(*OutcomeDefinitionExportView) error { return nil }); !errors.Is(err, ErrDefinitionExportIncomplete) {
				t.Fatalf("pending source=%v", err)
			}
			if window == "after-schema-commit" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if s.migrated {
					t.Fatal("fixture must lose in-memory migrated flag")
				}
			}
			if window == "after-tail-recovery" || window == "after-prefix-prepare" || window == "after-epoch-file" || window == "prepared-mismatch" {
				stored, err := s.journalPosition()
				if err != nil {
					t.Fatal(err)
				}
				log, err := metadatajournal.Open(s.MetadataJournalPath(), testJournalKey())
				if err != nil {
					t.Fatal(err)
				}
				state := JournalState{}
				if err := s.reconcileJournal(log, stored, &state, testJournalKey(), s.MetadataJournalPath()); err != nil {
					t.Fatal(err)
				}
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
				if window != "after-tail-recovery" {
					if err := s.prepareDefinitionExportEpoch(oldHead); err != nil {
						t.Fatal(err)
					}
				}
				if window == "after-epoch-file" || window == "prepared-mismatch" {
					previous := oldHead.Hash[:]
					if window == "prepared-mismatch" {
						previous = bytes.Repeat([]byte{8}, 32)
					}
					next, err := metadatajournal.StartEpoch(s.MetadataJournalPath(), testJournalKey(), metadatajournal.EpochHeader{Epoch: oldHead.Epoch + 1, PreviousEpoch: oldHead.Epoch, PreviousChainHead: previous, Reason: "test interrupted publication"})
					if err != nil {
						t.Fatal(err)
					}
					if err := next.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			key := testJournalKey()
			if window == "bad-key" {
				key = bytes.Repeat([]byte{1}, 32)
			}
			if window == "bad-native" {
				raw, err := os.ReadFile(s.MetadataJournalPath())
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(s.MetadataJournalPath(), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(s.MetadataJournalPath())
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.AttachMetadataJournal(key, "schema41")
			if window == "bad-key" || window == "bad-native" || window == "prepared-mismatch" {
				if err == nil {
					t.Fatal("unsafe epoch accepted")
				}
				after, readErr := os.ReadFile(s.MetadataJournalPath())
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("refused attach replaced journal")
				}
				if pending, err := s.definitionExportEpochPending(); err != nil || len(pending) == 0 {
					t.Fatalf("pending lost=%x err=%v", pending, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !state.StartedEpoch || state.Epoch != oldHead.Epoch+1 || state.Applied != 0 {
				t.Fatalf("state=%+v", state)
			}
			assertDefinitionExportRows(t, s, expected)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = openForTest(t, path)
			if err != nil {
				t.Fatal(err)
			}
			assertDefinitionExportRows(t, s, expected)
		})
	}
}

func TestOutcomeDefinitionFirstEpochFilePublicationRecoversPreparedPrefix(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	path := s.db.Path()
	if err := s.prepareDefinitionExportEpoch(metadatajournal.Head{}); err != nil {
		t.Fatal(err)
	}
	log, err := metadatajournal.StartEpoch(s.MetadataJournalPath(), testJournalKey(), metadatajournal.EpochHeader{Epoch: 1, Reason: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if state, err := s.AttachMetadataJournal(testJournalKey(), "retry"); err != nil || state.Epoch != 1 || !state.StartedEpoch {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	assertDefinitionExportRows(t, s, nil)
}

func TestOutcomeDefinitionPrefixPreparationRollsBackAfterRowFailure(t *testing.T) {
	path, _, _ := legacyDefinitionTailFixture(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stop := errors.New("interrupted baseline")
	if _, err := s.rebuildOutcomeDefinitionExportBounded(func(point string) error {
		if point == "before_definition_index_commit" {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		t.Fatalf("migration=%v", err)
	}
	if err := s.view(func(tx *Tx) error {
		head, err := definitionExportHead(tx)
		if err != nil {
			return err
		}
		if head.Count != 0 {
			t.Fatalf("failed baseline committed=%+v", head)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachMetadataJournal(testJournalKey(), "retry"); err != nil {
		t.Fatal(err)
	}
}

func TestOutcomeDefinitionExportFollowsReplicaConfirmedMetadataApply(t *testing.T) {
	primary, project, _ := definitionExportFixture(t, 0, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.db")
	if _, err := primary.Snapshot(path); err != nil {
		t.Fatal(err)
	}
	row := domain.OutcomeDefinition{ID: "odef_replica", ProjectID: project.ID, Name: "replica", Version: 1, DataType: domain.OutcomeBoolean, SuccessValues: []string{"true"}, CreatedAt: time.Now().UTC(), CreatedBy: "writer"}
	first, err := primary.PutOutcomeDefinition(context.Background(), row, project.Revision, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstSequence := primary.journal.Head().Sequence
	row = first
	row.Version++
	row.CreatedBy = "writer-next"
	second, err := primary.PutOutcomeDefinition(context.Background(), row, project.Revision, row.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	copyTestFile(t, primary.MetadataJournalPath(), filepath.Join(dir, MetadataJournalFileName))
	replica, err := OpenReplica(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	log, err := metadatajournal.OpenReplica(replica.MetadataJournalPath(), testJournalKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.AttachReplicaMetadataJournal(log, testJournalKey()); err != nil {
		log.Close()
		t.Fatal(err)
	}
	assertDefinitionExportRows(t, replica, nil)
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), log.Head().Epoch, firstSequence); err != nil {
		t.Fatal(err)
	}
	assertDefinitionExportRows(t, replica, []domain.OutcomeDefinition{first})
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), log.Head().Epoch, log.Head().Sequence); err != nil {
		t.Fatal(err)
	}
	assertDefinitionExportRows(t, replica, []domain.OutcomeDefinition{first, second})
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), log.Head().Epoch, log.Head().Sequence); err != nil {
		t.Fatal(err)
	}
	assertDefinitionExportRows(t, replica, []domain.OutcomeDefinition{first, second})
}
