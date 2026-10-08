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

func legacyProposalTailFixture(t *testing.T, version uint64, n int) (string, domain.DeploymentPriceProposal, metadatajournal.Head) {
	t.Helper()
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
	seedProposalPageFixture(t, s, "dep_proposals", n)
	revision := proposalRevisionForTest(t, s, "dep_proposals")
	if err := s.migrate(func(tx *Tx) error {
		for _, name := range [][]byte{bucketPricingProposalOrder, bucketPricingProposalCount} {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
		}
		if version < 44 {
			if err := tx.DeleteBucket(bucketPricingProposalRevision); err != nil {
				return err
			}
		}
		if version < 43 {
			if err := tx.DeleteBucket(bucketPricingAuditControl); err != nil {
				return err
			}
		}
		for v := version + 1; v <= schemaVersion; v++ {
			if err := tx.Bucket(bucketMigrationHistory).Delete(encodeUint64(v)); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(version))
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := proposalFixture("durable_unapplied", "dep_proposals", at)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	intent := pendingIntentFixture("durable_unapplied_audit", at)
	intentRaw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	ops := []metadatajournal.Op{
		{Kind: metadatajournal.OpPut, Path: []string{string(bucketDeploymentPriceProposals)}, Key: []byte(p.ID), Value: raw},
		{Kind: metadatajournal.OpPut, Path: []string{string(bucketPricingAuditIntents)}, Key: []byte(intent.EventID), Value: intentRaw},
	}
	if version >= 43 {
		ops = append(ops, metadatajournal.Op{Kind: metadatajournal.OpPut, Path: []string{string(bucketPricingAuditControl)}, Key: pricingPendingCountKey, Value: encodeUint64(1)})
	}
	if version >= 44 {
		ops = append(ops, metadatajournal.Op{Kind: metadatajournal.OpPut, Path: []string{string(bucketPricingProposalRevision)}, Key: []byte(p.DeploymentID), Value: encodeUint64(revision + 1)})
	}
	if _, err := s.journal.Append(ops); err != nil {
		t.Fatal(err)
	}
	head := s.journal.Head()
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path, p, head
}

func reconcileLegacyPricingForTest(t *testing.T, s *Store) {
	t.Helper()
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
		log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPricingMigrationAuthenticatesDurableTailBeforeIndexAndCount(t *testing.T) {
	for _, version := range []uint64{42, 44} {
		for _, window := range []string{"normal", "after-schema", "after-tail", "after-index-batch", "after-prepared", "after-epoch-file", "bad-key", "bad-journal"} {
			t.Run(fmt.Sprintf("schema%d/%s", version, window), func(t *testing.T) {
				path, p, oldHead := legacyProposalTailFixture(t, version, 130)
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { s.Close() }()
				if page, err := s.DeploymentPriceProposalsPage(context.Background(), p.DeploymentID, "", 1); !errors.Is(err, ErrProposalIndexIncomplete) || len(page.Items) != 0 {
					t.Fatal("incomplete schema exposed", page, err)
				}
				if window == "after-tail" || window == "after-index-batch" || window == "after-prepared" || window == "after-epoch-file" {
					reconcileLegacyPricingForTest(t, s)
					if window == "after-index-batch" {
						if err := s.rebuildPricingMigrationAfterJournal(func(point string) error {
							if point == "after_proposal_index_batch" {
								return errors.New("interrupted")
							}
							return nil
						}); err == nil {
							t.Fatal("block fault ignored")
						}
						if got := proposalCountForTest(t, s, p.DeploymentID); got != 64 {
							t.Fatal("first block was not finite/durable", got)
						}
					}
					if window == "after-prepared" || window == "after-epoch-file" {
						if err := s.prepareDefinitionExportEpoch(oldHead); err != nil {
							t.Fatal(err)
						}
					}
					if window == "after-epoch-file" {
						log, err := metadatajournal.StartEpoch(s.MetadataJournalPath(), testJournalKey(), metadatajournal.EpochHeader{Epoch: oldHead.Epoch + 1, PreviousEpoch: oldHead.Epoch, PreviousChainHead: oldHead.Hash[:], Reason: "interrupted pricing migration"})
						if err != nil {
							t.Fatal(err)
						}
						if err := log.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
				if window != "normal" && window != "bad-key" && window != "bad-journal" {
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					s, err = Open(path)
					if err != nil {
						t.Fatal(err)
					}
					if s.migrated {
						t.Fatal("lost-migrated crash window not exercised")
					}
				}
				key := testJournalKey()
				if window == "bad-key" {
					key = bytes.Repeat([]byte{3}, 32)
				}
				if window == "bad-journal" {
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
				state, err := s.AttachMetadataJournal(key, "pricing migration")
				if window == "bad-key" || window == "bad-journal" {
					if err == nil {
						t.Fatal("unauthenticated migration accepted")
					}
					after, readErr := os.ReadFile(s.MetadataJournalPath())
					if readErr != nil || !bytes.Equal(before, after) {
						t.Fatal("refusal changed journal", readErr)
					}
					if got := proposalCountForTest(t, s, p.DeploymentID); got != 0 {
						t.Fatal("index built before authentication", got)
					}
					return
				}
				if err != nil || !state.StartedEpoch || state.Epoch != oldHead.Epoch+1 {
					t.Fatal(state, err)
				}
				if got := proposalCountForTest(t, s, p.DeploymentID); got != 131 {
					t.Fatal("durable tail omitted", got)
				}
				pendingCountForTest(t, s, 1)
				page, err := s.DeploymentPriceProposalsPage(context.Background(), p.DeploymentID, "", 1)
				if err != nil || len(page.Items) != 1 || page.Items[0].ID != p.ID {
					t.Fatal(page, err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = openJournalledStore(t, path)
				page, err = s.DeploymentPriceProposalsPage(context.Background(), p.DeploymentID, "", 1)
				if err != nil || len(page.Items) != 1 || page.Items[0].ID != p.ID {
					t.Fatal("reopen lost rebuilt tail", page, err)
				}
			})
		}
	}
}

func TestPricingMigrationRebuildDiscardsUntrustedPartialContentsAndBoundsCleanup(t *testing.T) {
	path, p, _ := legacyProposalTailFixture(t, 44, 130)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reconcileLegacyPricingForTest(t, s)
	if err := s.rebuildPricingMigrationAfterJournal(func(point string) error {
		if point == "after_proposal_index_batch" {
			return errors.New("stop")
		}
		return nil
	}); err == nil {
		t.Fatal("fault ignored")
	}
	if err := s.migrate(func(tx *Tx) error {
		order := tx.Bucket(bucketPricingProposalOrder).Bucket([]byte(p.DeploymentID))
		if err := order.Put(make([]byte, 44), []byte("fake")); err != nil {
			return err
		}
		return tx.Bucket(bucketPricingProposalCount).Put([]byte(p.DeploymentID), encodeUint64(9000))
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
	done, err := s.clearPricingMigrationBlock(bucketPricingProposalOrder)
	if err != nil || done {
		t.Fatal("cleanup ignored finite row boundary", done, err)
	}
	if err := s.view(func(tx *Tx) error {
		order := tx.Bucket(bucketPricingProposalOrder).Bucket([]byte(p.DeploymentID))
		if order == nil || order.Stats().KeyN != 1 {
			t.Fatal("cleanup did not retire exactly64 keys")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachMetadataJournal(testJournalKey(), "retry partial"); err != nil {
		t.Fatal(err)
	}
	if got := proposalCountForTest(t, s, p.DeploymentID); got != 131 {
		t.Fatal("unsigned intermediate count trusted", got)
	}
	page, err := s.DeploymentPriceProposalsPage(context.Background(), p.DeploymentID, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != p.ID {
		t.Fatal(page, err)
	}
}

func TestPricingMigrationRow66FaultRetainsOnlyCompleteBlockAndRefusesReplicaSeed(t *testing.T) {
	path, p, _ := legacyProposalTailFixture(t, 44, 130)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reconcileLegacyPricingForTest(t, s)
	rows := 0
	err = s.rebuildPricingMigrationAfterJournal(func(point string) error {
		if point == "proposal_order_row" {
			rows++
			if rows == 66 {
				return errors.New("row66")
			}
		}
		return nil
	})
	if err == nil || proposalCountForTest(t, s, p.DeploymentID) != 64 {
		t.Fatal("partial second block committed", rows, err)
	}
	if _, err := s.PendingPricingAuditIntentCount(context.Background()); !errors.Is(err, ErrProposalIndexIncomplete) {
		t.Fatal("incomplete counter presented as available", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if primary, err := OpenPrimary(path); !errors.Is(err, ErrSchemaVersionMismatch) {
		if primary != nil {
			primary.Close()
		}
		t.Fatal("Primary opened unpublished migration", err)
	}
	replica, err := OpenReplica(path)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := metadatajournal.OpenReplica(replica.MetadataJournalPath(), testJournalKey())
	if err != nil {
		replica.Close()
		t.Fatal(err)
	}
	if _, err := replica.AttachReplicaMetadataJournal(journal, testJournalKey()); !errors.Is(err, ErrJournalDiverged) {
		t.Fatal("partial migration became seed", err)
	}
	journal.Close()
	replica.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AttachMetadataJournal(testJournalKey(), "retry row fault"); err != nil {
		t.Fatal(err)
	}
	if proposalCountForTest(t, s, p.DeploymentID) != 131 {
		t.Fatal("retry lost a complete source row")
	}
}

func TestPricingMigrationPreservesEarlierPreparedEpochAcrossSchemaUpgrade(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			path, p, oldHead := legacyProposalTailFixture(t, 44, 3)
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			reconcileLegacyPricingForTest(t, s)
			if err := s.prepareDefinitionExportEpoch(oldHead); err != nil {
				t.Fatal(err)
			}
			pending, err := s.definitionExportEpochPending()
			if err != nil || len(pending) != definitionExportPreparedBytes {
				t.Fatal(pending, err)
			}
			// Re-create the state a schema44 build left while publishing migration41.
			if err := s.migrate(func(tx *Tx) error {
				for _, name := range [][]byte{bucketPricingProposalOrder, bucketPricingProposalCount} {
					if err := tx.DeleteBucket(name); err != nil {
						return err
					}
				}
				if err := tx.Bucket(bucketMigrationHistory).Delete(encodeUint64(45)); err != nil {
					return err
				}
				return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(44))
			}); err != nil {
				t.Fatal(err)
			}
			if published {
				log, err := metadatajournal.StartEpoch(s.MetadataJournalPath(), testJournalKey(), metadatajournal.EpochHeader{Epoch: oldHead.Epoch + 1, PreviousEpoch: oldHead.Epoch, PreviousChainHead: oldHead.Hash[:], Reason: "earlier migration publication"})
				if err != nil {
					t.Fatal(err)
				}
				if err := log.Close(); err != nil {
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
			defer s.Close()
			after, err := s.definitionExportEpochPending()
			if err != nil || !bytes.Equal(pending, after) {
				t.Fatal("upgrade discarded epoch proof", after, err)
			}
			if _, err := s.AttachMetadataJournal(testJournalKey(), "schema45 upgrade"); err != nil {
				t.Fatal(err)
			}
			page, err := s.DeploymentPriceProposalsPage(context.Background(), p.DeploymentID, "", 1)
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != p.ID {
				t.Fatal(page, err)
			}
			pendingCountForTest(t, s, 1)
		})
	}
}

func TestPricingMigrationSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_PRICE_MIGRATION_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stored, err := s.journalPosition()
	if err != nil {
		t.Fatal(err)
	}
	log, err := metadatajournal.Open(s.MetadataJournalPath(), testJournalKey())
	if err != nil {
		t.Fatal(err)
	}
	head := log.Head()
	state := JournalState{}
	if err := s.reconcileJournal(log, stored, &state, testJournalKey(), s.MetadataJournalPath()); err != nil {
		log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		ready := filepath.Join(filepath.Dir(path), "pricing-migration-crash-ready")
		f, err := os.OpenFile(ready, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("durable-block")); err != nil {
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
		// Parent SIGKILLs this process with the DB still physically open.
		time.Sleep(30 * time.Second)
		t.Fatal("parent did not kill subprocess")
	}
	if os.Getenv("HALRO_PRICE_MIGRATION_CRASH_WINDOW") == "index-block" {
		if err := s.rebuildPricingMigrationAfterJournal(func(point string) error {
			if point == "after_proposal_index_batch" {
				stop()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := s.prepareDefinitionExportEpoch(head); err != nil {
			t.Fatal(err)
		}
		next, err := metadatajournal.StartEpoch(s.MetadataJournalPath(), testJournalKey(), metadatajournal.EpochHeader{Epoch: head.Epoch + 1, PreviousEpoch: head.Epoch, PreviousChainHead: head.Hash[:], Reason: "SIGKILL pricing publication"})
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
		stop()
	}
}

func TestPricingMigrationSIGKILLReopensDurableBlockAndEpochFile(t *testing.T) {
	for _, window := range []string{"index-block", "epoch-file"} {
		t.Run(window, func(t *testing.T) {
			path, p, oldHead := legacyProposalTailFixture(t, 44, 130)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestPricingMigrationSIGKILLHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "HALRO_PRICE_MIGRATION_CRASH_PATH="+path, "HALRO_PRICE_MIGRATION_CRASH_WINDOW="+window)
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
			ready := filepath.Join(filepath.Dir(path), "pricing-migration-crash-ready")
			deadline := time.After(10 * time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case err := <-exited:
					joined = true
					t.Fatal("child exited before durable window", err, output.String())
				case <-deadline:
					t.Fatal("child did not reach durable window")
				case <-ticker.C:
					if raw, err := os.ReadFile(ready); err == nil && string(raw) == "durable-block" {
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
				t.Fatal("child was not interrupted")
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if window == "index-block" && proposalCountForTest(t, s, p.DeploymentID) != 64 {
				t.Fatal("actual durable block missing")
			}
			if _, err := s.DeploymentPriceProposalsPage(context.Background(), p.DeploymentID, "", 1); !errors.Is(err, ErrProposalIndexIncomplete) {
				t.Fatal("crash exposed incomplete projection", err)
			}
			state, err := s.AttachMetadataJournal(testJournalKey(), "SIGKILL retry")
			if err != nil || !state.StartedEpoch || state.Epoch != oldHead.Epoch+1 {
				t.Fatal(state, err)
			}
			if proposalCountForTest(t, s, p.DeploymentID) != 131 {
				t.Fatal("crash retry lost proposals")
			}
			pendingCountForTest(t, s, 1)
			page, err := s.DeploymentPriceProposalsPage(context.Background(), p.DeploymentID, "", 1)
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != p.ID {
				t.Fatal(page, err)
			}
		})
	}
}
