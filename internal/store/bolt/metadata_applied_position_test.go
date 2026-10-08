package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestReadMetadataAppliedPositionSeedEpochAndRetirement(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	first, err := s.ReadMetadataAppliedPosition(ctx)
	if err != nil || first.Epoch != 0 || first.Sequence != 0 || first.Transaction == 0 {
		t.Fatal(first, err)
	}
	if err := s.writeJournalPosition(3, 7); err != nil {
		t.Fatal(err)
	}
	next, err := s.ReadMetadataAppliedPosition(ctx)
	if err != nil || next.Epoch != 3 || next.Sequence != 7 || next.Transaction <= first.Transaction {
		t.Fatal(next, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if pos, err := s.ReadMetadataAppliedPosition(canceled); !errors.Is(err, context.Canceled) || pos != (MetadataAppliedPosition{}) {
		t.Fatal(pos, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if pos, err := s.ReadMetadataAppliedPosition(ctx); !errors.Is(err, bbolt.ErrDatabaseNotOpen) || pos != (MetadataAppliedPosition{}) {
		t.Fatal(pos, err)
	}
	var absent *Store
	if pos, err := absent.ReadMetadataAppliedPosition(ctx); !errors.Is(err, bbolt.ErrDatabaseNotOpen) || pos != (MetadataAppliedPosition{}) {
		t.Fatal(pos, err)
	}
}

func TestReadMetadataAppliedPositionRejectsIncompleteAndMalformedKeys(t *testing.T) {
	for _, kind := range []string{"epoch-only", "sequence-only", "malformed-epoch", "malformed-sequence", "zero-epoch"} {
		t.Run(kind, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.writeJournalPosition(3, 7); err != nil {
				t.Fatal(err)
			}
			err = s.db.Update(func(tx *bbolt.Tx) error {
				meta := tx.Bucket(bucketMeta)
				switch kind {
				case "epoch-only":
					return meta.Delete(keyAppliedJournalSequence)
				case "sequence-only":
					return meta.Delete(keyMetadataJournalEpoch)
				case "malformed-epoch":
					return meta.Put(keyMetadataJournalEpoch, []byte("bad"))
				case "malformed-sequence":
					return meta.Put(keyAppliedJournalSequence, []byte("bad"))
				case "zero-epoch":
					return meta.Put(keyMetadataJournalEpoch, encodeUint64(0))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if pos, err := s.ReadMetadataAppliedPosition(context.Background()); err == nil || pos != (MetadataAppliedPosition{}) {
				t.Fatal("malformed position supplied a usable cut", pos, err)
			}
		})
	}
}
