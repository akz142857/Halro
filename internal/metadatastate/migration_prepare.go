package metadatastate

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// MigrationReceipt describes an authenticated private candidate. It is not a
// common-cut selection, bootstrap identity, promotion or live publish grant.
type MigrationReceipt struct {
	Version    uint8                              `json:"version"`
	Evidence   BaselineEvidence                   `json:"evidence"`
	Root       authenticatedstate.RootIdentity    `json:"root"`
	Head       metadatajournal.Head               `json:"head"`
	SourceHead metadatajournal.Head               `json:"source_head"`
	Reference  AuthenticatedMetadataSealReference `json:"reference"`
}

// MigrationPreparation retains actual cleanup owners on every uncertain exit.
// Close preserves successful candidate files; Discard removes only owned files.
type MigrationPreparation struct {
	mu       sync.Mutex
	baseline *Baseline
	source   *backup.VerifiedFile
	work     resourcebudget.Permit
	closeErr error
	Receipt  MigrationReceipt
}

// MigrationPreparationFailure retains the actual cleanup-only owner through
// callers (including CLI) that can return only an error.
type MigrationPreparationFailure struct {
	Cause error
	Owner *MigrationPreparation
}

// WithBaseline lends the actual private preparation owner for a complete
// common publication while retaining its cleanup responsibility. A receipt
// copied from another candidate cannot supply this source.
func (p *MigrationPreparation) WithBaseline(ctx context.Context, visit func(*Baseline) error) error {
	if p == nil || visit == nil {
		return ErrSource
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.closeErr != nil || p.work != (resourcebudget.Permit{}) || p.baseline == nil || p.Receipt.Version != 1 {
		return errors.Join(ErrSource, p.closeErr)
	}
	return visit(p.baseline)
}

func (e *MigrationPreparationFailure) Error() string { return e.Cause.Error() }
func (e *MigrationPreparationFailure) Unwrap() error { return e.Cause }

func (p *MigrationPreparation) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.work != (resourcebudget.Permit{}) {
		return p.closeErr
	}
	p.closeErr = errors.Join(p.closeErr, p.baseline.Close(), p.source.Discard())
	return p.closeErr
}
func (p *MigrationPreparation) Discard() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.work != (resourcebudget.Permit{}) {
		return p.closeErr
	}
	p.closeErr = errors.Join(p.closeErr, p.baseline.Discard(), p.source.Discard())
	return p.closeErr
}

// PrepareFormat2FromBackup is the offline Metadata migration preparation
// stage. Both source payloads come from one authenticated backup; the complete
// format1 journal must exactly match that backup's Metadata applied position.
// It creates a private format2 epoch and never replaces live files. Independent
// common selection and default Runtime bootstrap remain caller obligations.
func PrepareFormat2FromBackup(ctx context.Context, archive *backup.VerifiedArchive, ledgerKey, sourceJournalKey []byte, dataDir, workspace string, guard *lock.Lock, o BaselineOptions, maxJournalBytes int64) (result *MigrationPreparation, resultErr error) {
	if archive == nil || o.Budget == nil || guard == nil || !guard.CoversDataDirectory(dataDir) || !filepath.IsAbs(workspace) || len(ledgerKey) != 32 || len(sourceJournalKey) != metadatajournal.KeySize || o.MaxSourceBytes <= 0 || maxJournalBytes < 4096 {
		return nil, ErrSource
	}
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(ErrSource, err)
	}
	p := &MigrationPreparation{}
	defer func() {
		if resultErr != nil {
			if cleanup := p.Discard(); cleanup != nil {
				result, resultErr = p, errors.Join(resultErr, cleanup)
			} else {
				result = nil
			}
		}
	}()
	p.source, err = archive.OpenVerifiedFile(ctx, "data/metadata.journal", filepath.Join(workspace, "format1-source.journal"), o.MaxSourceBytes, o.Budget)
	if err != nil {
		return nil, err
	}
	p.work, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: BaselineSourceRAMBytes})
	if err != nil {
		return nil, err
	}
	var sourceHead metadatajournal.Head
	err = p.source.WithReadOnly(ctx, func(ctx context.Context, file *os.File, _ backup.File) error {
		var err error
		sourceHead, err = metadatajournal.ProbeExactContext(ctx, file.Name(), sourceJournalKey, o.MaxSourceBytes)
		return err
	})
	if errors.Is(err, ownedfile.ErrRetirement) {
		p.closeErr = err
		return p, err
	}
	releaseErr := p.work.Release()
	if releaseErr != nil {
		p.closeErr = errors.Join(err, releaseErr)
		return p, p.closeErr
	}
	p.work = resourcebudget.Permit{}
	if err != nil {
		return nil, err
	}
	if sourceHead.FormatVersion != 1 {
		return nil, metadatajournal.ErrIncompatibleFormat
	}
	metadata := archive.Summary().Metadata
	if sourceHead.Epoch != metadata.MetadataJournalEpoch || sourceHead.Sequence != metadata.MetadataJournalSequence || sourceHead.Epoch == math.MaxUint64 || !guard.CoversDataDirectory(dataDir) {
		return nil, ErrSource
	}
	if err := p.source.Discard(); err != nil {
		return nil, err
	}
	p.source = nil
	p.baseline, err = BuildFromBackup(ctx, archive, ledgerKey, filepath.Join(workspace, "metadata-source.db"), filepath.Join(workspace, "metadata-root"), o)
	if err != nil {
		return nil, err
	}
	head, err := p.baseline.StartNewEpoch(ctx, ledgerKey, filepath.Join(workspace, "metadata.journal"), sourceHead.Epoch+1, maxJournalBytes)
	if err != nil {
		return nil, err
	}
	root, err := p.baseline.store.RootIdentity(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := p.baseline.SealReference(ctx)
	if err != nil {
		return nil, err
	}
	if !guard.CoversDataDirectory(dataDir) {
		return nil, ErrSource
	}
	p.Receipt = MigrationReceipt{Version: 1, Evidence: p.baseline.Evidence(), Root: root, Head: head, SourceHead: sourceHead, Reference: ref}
	return p, nil
}
