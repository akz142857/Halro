package metadatastate

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const EpochBuildRAMBytes int64 = 64 << 20

// StartNewEpoch consumes the private imported candidate's one fresh-epoch
// transition. It normalizes bookkeeping in one authenticated root transaction,
// then exclusively creates a format2 native header bound to the resulting full
// root. It never replaces a live DB/journal or accepts a role/confirmed prefix.
// A failure after mutation permanently retires the candidate for Discard.
// BaselineEvidence remains the original backup source, not the new position.
func (b *Baseline) StartNewEpoch(ctx context.Context, ledgerKey []byte, path string, epoch uint64, maxJournalBytes int64) (result metadatajournal.Head, resultErr error) {
	return b.startNewEpoch(ctx, ledgerKey, path, epoch, maxJournalBytes, nil)
}
func (b *Baseline) startNewEpoch(ctx context.Context, ledgerKey []byte, path string, epoch uint64, maxJournalBytes int64, phase func(string)) (result metadatajournal.Head, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return result, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() || b.linked {
		return result, authenticatedstate.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(ledgerKey) != 32 || sha256.Sum256(ledgerKey) != b.ledgerKeyTag || epoch <= b.evidence.Metadata.MetadataJournalEpoch || epoch == 0 || maxJournalBytes < 4096 || !filepath.IsAbs(path) || filepath.Clean(filepath.Dir(path)) == filepath.Clean(b.output) {
		return result, ErrSource
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return result, errors.Join(ErrSource, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return result, errors.Join(ErrSource, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	permit, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: EpochBuildRAMBytes})
	if err != nil {
		return result, err
	}
	defer func() {
		releaseErr := permit.Release()
		resultErr = errors.Join(resultErr, releaseErr)
		if releaseErr != nil {
			b.ready.Store(false)
		}
		if resultErr != nil {
			result = metadatajournal.Head{}
		}
	}()
	disk, err := b.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: maxJournalBytes})
	if err != nil {
		return result, err
	}
	b.journalDisk, b.journalCharged = disk, true
	b.ready.Store(false)
	defer func() {
		if resultErr == nil {
			b.ready.Store(true)
		} else {
			result = metadatajournal.Head{}
		}
	}()
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		return result, err
	}
	key, err := hkdf.Key(sha256.New, ledgerKey, root.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return result, err
	}
	defer clear(key)
	cutBytes := append([]byte("halro:metadata:epoch-baseline-cut:v1\x00"), root.Cut[:]...)
	cutBytes = binary.BigEndian.AppendUint64(cutBytes, epoch)
	cut := sha256.Sum256(cutBytes)
	if _, err := b.store.Update(ctx, cut, func(tx *authenticatedstate.Transaction) error {
		for _, entry := range []struct {
			name  string
			value uint64
		}{{"metadata_journal_epoch", epoch}, {"applied_journal_sequence", 0}} {
			k, err := RowKey([][]byte{[]byte("meta")}, []byte(entry.name), false)
			if err != nil {
				return err
			}
			if err := tx.Put(k, binary.BigEndian.AppendUint64(nil, entry.value)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return result, err
	}
	root, err = b.store.RootIdentity(ctx)
	if err != nil {
		return result, err
	}
	if phase != nil {
		phase("root-normalized")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	e := b.evidence
	bound := metadatajournal.BaselineBinding{Version: 1, Epoch: epoch, Identity: e.Identity, KeyGeneration: e.KeyGeneration, Schema: e.Metadata.SchemaVersion, Catalog: e.CatalogDigest, Scope: root.Scope, Cut: root.Cut, RootDigest: root.Digest, RootID: root.ID, Rows: root.Rows}
	b.journal, err = metadatajournal.StartBoundEpoch(ctx, path, key, metadatajournal.EpochHeader{Epoch: epoch, PreviousEpoch: e.Metadata.MetadataJournalEpoch, Reason: "authenticated-backup-baseline", Baseline: &bound, FileRevision: 1}, maxJournalBytes)
	if err != nil {
		return result, err
	}
	if phase != nil {
		phase("journal-durable")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	b.journalPath = path
	b.linked = true
	b.position = b.journal.Head()
	return b.position, nil
}

// EpochHead is a fixed binding/head receipt, not permission to append, resume,
// trim, promote or use this imported node's derived rows. Complete seal/role
// selection and qualified cross-process recovery remain before Runtime use.
func (b *Baseline) EpochHead() (metadatajournal.Head, error) {
	if b == nil || !b.mu.TryLock() {
		return metadatajournal.Head{}, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() || !b.linked || b.journal == nil {
		return metadatajournal.Head{}, authenticatedstate.ErrClosed
	}
	if b.selectedStartup {
		if err := b.journal.HealthError(); err != nil {
			return metadatajournal.Head{}, err
		}
	}
	return b.journal.Head(), nil
}
