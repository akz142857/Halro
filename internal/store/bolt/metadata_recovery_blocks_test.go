package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func metadataAtomicTailFixture(t *testing.T, n int, badLast bool) (string, uint64, metadatajournal.Head) {
	t.Helper()
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	baseline := s.journal.Head().Sequence
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("recovery_%03d", i)
		raw, err := json.Marshal(domain.Project{ID: id, Name: id, Enabled: true, Revision: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{string(bucketProjects)}, Key: []byte(id), Value: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	if badLast {
		if _, err := s.journal.Append([]metadatajournal.Op{
			{Kind: metadatajournal.OpPut, Path: []string{string(bucketProjects)}, Key: []byte("failed_frame_first_op"), Value: []byte(`{"id":"failed_frame_first_op"}`)},
			{Kind: metadatajournal.OpPut, Path: []string{"missing_recorded_bucket"}, Key: []byte("impossible"), Value: []byte("value")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	head := s.journal.Head()
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path, baseline, head
}

func TestMetadataRecoveryCommitsCompleteFramesAndResumesAfterFailure(t *testing.T) {
	path, baseline, head := metadataAtomicTailFixture(t, 130, false)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.journalPosition()
	if err != nil {
		t.Fatal(err)
	}
	log, err := metadatajournal.Open(s.MetadataJournalPath(), testJournalKey())
	if err != nil {
		t.Fatal(err)
	}
	state := JournalState{}
	stop := errors.New("after-frame")
	err = s.replayJournal(log, stored, head, &state, func(sequence uint64) error {
		if sequence == baseline+64 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || state.Applied != baseline+64 || state.Replayed != 64 {
		t.Fatal(state, err)
	}
	actual, err := s.journalPosition()
	if err != nil || actual.sequence != baseline+64 {
		t.Fatal(actual, err)
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
	state, err = s.AttachMetadataJournal(testJournalKey(), "resume exact frame")
	if err != nil || state.Applied != head.Sequence || state.Replayed != 66 {
		t.Fatal(state, err)
	}
	for i := 0; i < 130; i++ {
		if _, err := s.GetProject(context.Background(), fmt.Sprintf("recovery_%03d", i)); err != nil {
			t.Fatal(i, err)
		}
	}
}

func TestMetadataRecoveryBadFrameRollsBackAllItsOpsAndPosition(t *testing.T) {
	path, baseline, head := metadataAtomicTailFixture(t, 2, true)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.AttachMetadataJournal(testJournalKey(), "bad frame")
	if err == nil || state.Applied != baseline+2 || state.Replayed != 2 {
		t.Fatal(state, err)
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte("failed_frame_first_op")) != nil {
			t.Fatal("partial native frame committed")
		}
		sequence, _ := decodeUint64(tx.Bucket(bucketMeta).Get(keyAppliedJournalSequence))
		if sequence != head.Sequence-1 {
			t.Fatal("bad frame advanced position", sequence)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.AttachMetadataJournal(testJournalKey(), "bad frame retry")
	if err == nil || state.Replayed != 0 || state.Applied != baseline+2 {
		t.Fatal("retry reapplied complete prefix", state, err)
	}
}

func TestDefinitionMigrationCommitsFinitePrefixAndRebuildsAfterReopen(t *testing.T) {
	s, _, expected := definitionExportFixture(t, 130, 1)
	path := s.path
	stop := errors.New("after-first-definition-block")
	_, err := s.rebuildOutcomeDefinitionExportBounded(func(point string) error {
		if point == "after_definition_index_batch" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if err := s.view(func(tx *Tx) error {
		head, err := definitionExportHead(tx)
		if err != nil {
			return err
		}
		if head.Count != 64 {
			t.Fatal("block did not contain64 facts", head)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithOutcomeDefinitionExport(context.Background(), func(*OutcomeDefinitionExportView) error { return nil }); !errors.Is(err, ErrDefinitionExportIncomplete) {
		t.Fatal("partial prefix exposed", err)
	}
	// A forged intermediate count/head cannot obtain readiness on retry.
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketOutcomeDefinitionExportMeta).Put(definitionExportHeadKey, bytes.Repeat([]byte{9}, 40))
	}); err != nil {
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
	if _, err := s.AttachMetadataJournal(testJournalKey(), "definition retry"); err != nil {
		t.Fatal(err)
	}
	assertDefinitionExportRows(t, s, expected)
}

func TestDefinitionMigrationPreservesWideNativeRowAndValidatesOutsideReader(t *testing.T) {
	s, project, _ := definitionExportFixture(t, 0, 1)
	row := domain.OutcomeDefinition{ID: "odef_widenative", ProjectID: project.ID, Name: "wide", Version: 1, Revision: 1, DataType: domain.OutcomeBoolean, SuccessValues: []string{"true"}, CreatedAt: time.Now().UTC(), CreatedBy: "system"}
	if err := row.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"unknown":"`)...)
	// At the exact existing raw limit; the separate native key must not subtract
	// from it, as the pricing aggregate limit would incorrectly do.
	raw = append(raw, bytes.Repeat([]byte("x"), outcomeDefinitionExportRowBytes-len(raw)-2)...)
	raw = append(raw, []byte(`"}`)...)
	if len(raw) != outcomeDefinitionExportRowBytes {
		t.Fatal(len(raw))
	}
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketOutcomeDefinitions).Put(outcomeDefinitionKey(row.ID, 1), raw)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.rebuildOutcomeDefinitionExportBounded(func(point string) error {
		if point == "definition_export_row" {
			// A writer must be able to run while validation happens: no reader pins
			// the source across decode. An admitted source mutation is then rejected
			// by the exact transaction fence before any page can commit.
			return s.migrate(func(tx *Tx) error { return tx.Bucket(bucketMeta).Put([]byte("wide_row_test_point"), []byte("writer")) })
		}
		return nil
	})
	if !errors.Is(err, ErrDefinitionExportCorrupt) {
		t.Fatal("mutation during decode was not fenced", err)
	}
	if _, err := s.rebuildOutcomeDefinitionExportBounded(nil); err != nil {
		t.Fatal("valid raw-wide row refused", err)
	}
}

func TestDefinitionPreparedFinishRebuildsMiddleIndexAndRejectsChangedSource(t *testing.T) {
	for _, change := range []string{"index", "source"} {
		t.Run(change, func(t *testing.T) {
			path, _, oldHead := legacyDefinitionTailFixture(t)
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			reconcileLegacyPricingForTest(t, s)
			if err := s.prepareDefinitionExportEpoch(oldHead); err != nil {
				t.Fatal(err)
			}
			if err := s.migrate(func(tx *Tx) error {
				if change == "index" {
					index := tx.Bucket(bucketOutcomeDefinitionExport)
					raw := bytes.Clone(index.Get(encodeUint64(1)))
					raw[len(raw)-64] ^= 1
					return index.Put(encodeUint64(1), raw)
				}
				source := tx.Bucket(bucketOutcomeDefinitions)
				key, raw := source.Cursor().First()
				var row domain.OutcomeDefinition
				if err := json.Unmarshal(raw, &row); err != nil {
					return err
				}
				row.CreatedBy = "valid-but-not-prepared"
				changed, err := json.Marshal(row)
				if err != nil {
					return err
				}
				return source.Put(key, changed)
			}); err != nil {
				t.Fatal(err)
			}
			log, err := metadatajournal.StartEpoch(s.MetadataJournalPath(), testJournalKey(), metadatajournal.EpochHeader{Epoch: oldHead.Epoch + 1, PreviousEpoch: oldHead.Epoch, PreviousChainHead: oldHead.Hash[:], Reason: "prepared-definition test"})
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
			_, err = s.AttachMetadataJournal(testJournalKey(), "prepared retry")
			if change == "source" {
				if !errors.Is(err, ErrDefinitionExportCorrupt) {
					t.Fatal("changed prepared source accepted", err)
				}
				if pending, err := s.definitionExportEpochPending(); err != nil || len(pending) == 0 {
					t.Fatal("failed prepared gate cleared", pending, err)
				}
				return
			}
			if err != nil {
				t.Fatal("untrusted intermediate index not rebuilt", err)
			}
			if err := s.WithOutcomeDefinitionExport(context.Background(), func(v *OutcomeDefinitionExportView) error {
				rows, err := readAllDefinitionExport(context.Background(), v)
				if err != nil || len(rows) != 3 {
					t.Fatal(rows, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataRecoveryCloseJoinsActualFrameCallback(t *testing.T) {
	path, _, head := metadataAtomicTailFixture(t, 3, false)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.journalPosition()
	if err != nil {
		t.Fatal(err)
	}
	log, err := metadatajournal.Open(s.MetadataJournalPath(), testJournalKey())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		state := JournalState{}
		done <- s.replayJournal(log, stored, head, &state, func(uint64) error { close(entered); <-release; return nil })
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	<-s.lifetime.ctx.Done()
	select {
	case err := <-closed:
		close(release)
		t.Fatal("close returned before actual frame callback", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestMetadataRecoveryBlocksSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_METADATA_BLOCK_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var s *Store
	var err error
	window := os.Getenv("HALRO_METADATA_BLOCK_CRASH_WINDOW")
	if window == "replica-frame" {
		s = openReplicaBlockFixture(t, path)
	} else {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	stop := func() {
		f, err := os.OpenFile(filepath.Join(filepath.Dir(path), "metadata-block-crash-ready"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("durable")); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second)
		t.Fatal("parent did not kill child")
	}
	if window == "replica-frame" {
		stored, err := s.journalPosition()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.applyReplicaMetadataThrough(context.Background(), s.journal.Head().Epoch, stored.sequence+100, func(sequence uint64) error {
			if sequence == stored.sequence+64 {
				stop()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	} else if window == "native-frame" {
		stored, err := s.journalPosition()
		if err != nil {
			t.Fatal(err)
		}
		log, err := metadatajournal.Open(s.MetadataJournalPath(), testJournalKey())
		if err != nil {
			t.Fatal(err)
		}
		state := JournalState{}
		if err := s.replayJournal(log, stored, log.Head(), &state, func(uint64) error {
			if state.Replayed == 64 {
				stop()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := s.rebuildOutcomeDefinitionExportBounded(func(point string) error {
			if point == "after_definition_index_batch" {
				stop()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMetadataRecoveryBlocksSIGKILLPersistsNativePositionAndHiddenDefinitionPrefix(t *testing.T) {
	for _, window := range []string{"native-frame", "definition-block", "replica-frame"} {
		t.Run(window, func(t *testing.T) {
			var path string
			var baseline uint64
			var expected []domain.OutcomeDefinition
			if window != "definition-block" {
				path, baseline, _ = metadataAtomicTailFixture(t, 130, false)
			} else {
				s, _, rows := definitionExportFixture(t, 130, 1)
				path, expected = s.path, rows
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestMetadataRecoveryBlocksSIGKILLHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "HALRO_METADATA_BLOCK_CRASH_PATH="+path, "HALRO_METADATA_BLOCK_CRASH_WINDOW="+window)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			joined := false
			defer func() {
				if !joined {
					cmd.Process.Kill()
					<-exited
				}
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			deadline := time.After(10 * time.Second)
			for {
				select {
				case err := <-exited:
					joined = true
					t.Fatal("child exited before commit", err, output.String())
				case <-deadline:
					t.Fatal("child did not reach durable block")
				case <-ticker.C:
					if raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "metadata-block-crash-ready")); err == nil && string(raw) == "durable" {
						goto kill
					}
				}
			}
		kill:
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitErr := <-exited
			joined = true
			if waitErr == nil || cmd.ProcessState.Success() {
				t.Fatal("child was not killed")
			}
			var s *Store
			if window == "replica-frame" {
				s = openReplicaBlockFixture(t, path)
			} else {
				s, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer s.Close()
			if window == "replica-frame" {
				stored, err := s.journalPosition()
				if err != nil || stored.sequence != baseline+64 {
					t.Fatal(stored, err)
				}
				n, err := s.ApplyReplicaMetadataThrough(context.Background(), s.journal.Head().Epoch, baseline+100)
				if err != nil || n != baseline+100 {
					t.Fatal(n, err)
				}
				if err := s.view(func(tx *Tx) error {
					b := tx.Bucket(bucketProjects)
					if b.Get([]byte("recovery_099")) == nil || b.Get([]byte("recovery_100")) != nil {
						t.Fatal("wrong confirmed prefix after SIGKILL")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			} else if window == "native-frame" {
				stored, err := s.journalPosition()
				if err != nil || stored.sequence != baseline+64 {
					t.Fatal(stored, err)
				}
				state, err := s.AttachMetadataJournal(testJournalKey(), "SIGKILL native-frame retry")
				if err != nil || state.Replayed != 66 || state.Applied != baseline+130 {
					t.Fatal(state, err)
				}
			} else {
				if err := s.WithOutcomeDefinitionExport(context.Background(), func(*OutcomeDefinitionExportView) error { return nil }); !errors.Is(err, ErrDefinitionExportIncomplete) {
					t.Fatal("killed block exposed", err)
				}
				if err := s.view(func(tx *Tx) error {
					head, err := definitionExportHead(tx)
					if err != nil {
						return err
					}
					if head.Count != 64 {
						t.Fatal(head)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AttachMetadataJournal(testJournalKey(), "SIGKILL definition retry"); err != nil {
					t.Fatal(err)
				}
				assertDefinitionExportRows(t, s, expected)
			}
		})
	}
}
