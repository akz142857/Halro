// Package metadatastate constructs Metadata content roots from authenticated
// backup sources and reopens independently qualified component roots. It grants
// no default Runtime startup, role/epoch acceptance or HA authority.
package metadatastate

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	bbolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

var ErrSource = errors.New("metadata baseline source or identity is not eligible")

const BaselineSourceRAMBytes int64 = 32 << 20

type BaselineOptions struct {
	// Established independently of the backup/live Metadata. Identity and key
	// generation scope the stable subsystem key; InstanceID must match its row.
	Identity                                     [32]byte
	InstanceID                                   string
	KeyGeneration                                uint64
	Budget                                       *resourcebudget.Manager
	MaxSourceBytes, MaxDiskBytes, MaxAppendBytes int64
}

// ValidatePublicationPaths checks the actual private owner, not a copied
// migration receipt, before an application makes it a normal startup source.
func (b *Baseline) ValidatePublicationPaths(root, journal string) error {
	if b == nil {
		return ErrSource
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ready.Load() || !b.linked || filepath.Clean(root) != b.output || filepath.Clean(journal) != b.journalPath {
		return ErrSource
	}
	return nil
}

// BaselineEvidence describes content authenticity at this backup's cut. It
// does not prove earlier live-DB history, freshness, a current role/term, key
// compatibility after rotation, or a journal epoch's initial baseline link.
type BaselineEvidence struct {
	Version          uint8
	Identity         [32]byte
	InstanceID       string
	KeyGeneration    uint64
	BackupID         string
	CiphertextDigest [32]byte
	ManifestDigest   string
	Metadata         boltstore.MetadataInfo
	File             backup.File
	CatalogDigest    [32]byte
	// Rows is the imported baseline root row count, including this provenance
	// receipt. Starting a new epoch may add previously absent bookkeeping rows.
	Rows uint64
}

// baselineEvidenceKey is outside RowKey's versioned 0x01 namespace. Its
// authenticated value preserves the backup provenance needed after the
// importer process exits. It sorts after every source row, so it can be
// appended without buffering or a second source pass.
var baselineEvidenceKey = []byte{0xff}

type Baseline struct {
	haPeerTransition            *haPeerTransitionSource
	haPhysicalSource            *haPhysicalSource
	physicalRecovery            *TerminalPhysicalWindow
	physicalPinRecoveryVerified bool
	physicalWindows             [resourcebudget.MaxCompletionBatch]*TerminalPhysicalWindow
	physicalEnabled             bool
	physicalDebt                int
	selectedPublisher           *authenticatedstate.SealCandidatePublisher
	selectedGuard               *lock.Lock
	readOnly                    bool
	haPrimaryPrepared           *metadatajournal.BoundReplicationAuthority
	haReplicaPrepared           *metadatajournal.BoundReplicationAuthority
	readSource                  *authenticatedstate.Store
	readCloneParent             string
	readCloneInfo               os.FileInfo
	readCloneLock               *ownedfile.Owner
	businessOnce                sync.Once
	businessLane                chan struct{}
	businessWaiters             atomic.Int32
	sealDebt                    resourcebudget.Permit
	transactionDebt             resourcebudget.Permit
	mu                          sync.Mutex
	ctx                         context.Context
	cancel                      context.CancelFunc
	budget                      *resourcebudget.Manager
	ledgerKeyTag                [32]byte
	output                      string
	journal                     *metadatajournal.Log
	journalDisk                 resourcebudget.Permit
	journalCharged              bool
	borrowed                    bool
	borrowedClosed              bool
	borrowedClean               bool
	rotationSource              bool // fully qualified historical cleanup owner, no business admission
	closeErr                    error
	retirementErr               error
	retiredRewrite              resourcebudget.Permit
	journalPath                 string
	linked                      bool
	position                    metadatajournal.Head
	trimState                   metadatajournal.TrimStateBinding
	store                       *authenticatedstate.Store
	source                      *backup.VerifiedFile // retained only when physical source cleanup fails
	evidence                    BaselineEvidence
	ready                       atomic.Bool
	selectedStartup             bool
	startupCancel               context.CancelFunc
	startupStop                 func() bool
}

// BuildFromBackup streams every bucket (including empty/nested buckets and its
// sequence) and every raw value into one authenticated content root. All classes
// A/B/C/D/E/bookkeeping are included; it does not grant Replica credit to the
// source node's C/E rows. No unknown entry or oversized row is silently omitted.
// The only accepted source is an OpenVerified archive, never a live DB path.
// sourcePath is an absent file in a private directory; output is an absent root
// directory. On cleanup failure a non-nil cleanup-only result accompanies err.
func BuildFromBackup(ctx context.Context, archive *backup.VerifiedArchive, ledgerKey []byte, sourcePath, output string, o BaselineOptions) (result *Baseline, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if archive == nil || len(ledgerKey) != 32 || o.Identity == ([32]byte{}) || o.InstanceID == "" || len(o.InstanceID) > 128 || o.KeyGeneration == 0 || o.Budget == nil || o.MaxSourceBytes <= 0 || o.MaxDiskBytes <= 0 || o.MaxAppendBytes <= 0 || !filepath.IsAbs(sourcePath) || !filepath.IsAbs(output) {
		return nil, ErrSource
	}
	root := archive.Summary()
	if root.BackupID == "" || len(root.BackupID) > 128 || root.Metadata.SchemaVersion != boltstore.CurrentSchemaVersion() || len(root.Metadata.MinimumLedgerReaderVersion) > 128 {
		return nil, ErrSource
	}
	ciphertext, err := archive.CiphertextDigest()
	if err != nil {
		return nil, err
	}
	manifest, err := archive.ManifestDigest()
	if err != nil {
		return nil, err
	}
	b := &Baseline{budget: o.Budget, ledgerKeyTag: sha256.Sum256(ledgerKey), output: output, evidence: BaselineEvidence{Version: 1, Identity: o.Identity, InstanceID: o.InstanceID, KeyGeneration: o.KeyGeneration, BackupID: root.BackupID, CiphertextDigest: ciphertext, ManifestDigest: manifest, Metadata: root.Metadata, CatalogDigest: boltstore.MetadataBaselineCatalogDigest()}}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	defer func() {
		if resultErr != nil {
			if cleanup := b.Discard(); cleanup != nil {
				result, resultErr = b, errors.Join(resultErr, cleanup)
			} else {
				result = nil
			}
		}
	}()
	b.source, err = archive.OpenVerifiedFile(ctx, "data/metadata.db", sourcePath, o.MaxSourceBytes, o.Budget)
	if err != nil {
		return nil, err
	}
	scope := baselineScope(o)
	key, err := hkdf.Key(sha256.New, ledgerKey, scope[:], "halro:metadata:authenticated-baseline:hmac:v1", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	err = b.source.WithReadOnly(ctx, func(ctx context.Context, input *os.File, fact backup.File) (callbackErr error) {
		// Conservatively charge the complete source mapping too. A fixed cursor
		// is not evidence that mmap resident pages stay below a fixed RAM fee.
		// Large legacy DBs may refuse this offline importer; Linux calibration
		// and a production migration profile are separate completion conditions.
		if fact.Size < 0 || fact.Size > math.MaxInt64-BaselineSourceRAMBytes {
			return ErrSource
		}
		permit, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: BaselineSourceRAMBytes + fact.Size})
		if err != nil {
			return err
		}
		defer func() {
			if !errors.Is(callbackErr, ownedfile.ErrRetirement) {
				callbackErr = errors.Join(callbackErr, permit.Release())
			}
		}()
		// bbolt gets a duplicate of the authenticated, pinned, read-only FD. Its
		// OpenFile hook never resolves a caller-controlled replacement pathname.
		db, err := bbolt.Open(input.Name(), 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second, PreLoadFreelist: false, OpenFile: func(_ string, flags int, _ os.FileMode) (*os.File, error) {
			if flags != os.O_RDONLY {
				return nil, ErrSource
			}
			fd, err := unix.Dup(int(input.Fd()))
			if err != nil {
				return nil, err
			}
			unix.CloseOnExec(fd)
			return os.NewFile(uintptr(fd), input.Name()), nil
		}})
		if err != nil {
			return err
		}
		defer func() { callbackErr = errors.Join(callbackErr, db.Close()) }()
		return db.View(func(tx *bbolt.Tx) error {
			if err := verifyMetadataIdentity(tx, root.Metadata, o.InstanceID); err != nil {
				return err
			}
			b.evidence.File = fact
			cut, err := baselineCut(scope, b.evidence)
			if err != nil {
				return err
			}
			iterator := newBaselineIterator(ctx, tx)
			evidenceAdded := false
			b.store, err = authenticatedstate.CreateStore(ctx, output, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: cut, Key: key, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
				row, found, err := iterator.next()
				if found && err == nil {
					b.evidence.Rows++
					return row, true, nil
				}
				if err != nil || found || evidenceAdded {
					return row, found, err
				}
				evidenceAdded = true
				encoded, err := json.Marshal(b.evidence)
				if err != nil {
					return authenticatedstate.Row{}, false, err
				}
				return authenticatedstate.Row{Key: append([]byte(nil), baselineEvidenceKey...), Value: encoded}, true, nil
			})
			if err != nil {
				return err
			}
			root, err := b.store.RootIdentity(ctx)
			if err != nil {
				return err
			}
			b.evidence.Rows = root.Rows
			finalCut, err := baselineCut(scope, b.evidence)
			if err != nil {
				return err
			}
			return b.persistEvidence(ctx, finalCut)
		})
	})
	if err != nil {
		return nil, err
	}
	// Candidate stays unpublished through actual bbolt Close, source recheck,
	// source descriptor retirement, identity-checked unlink and directory sync.
	if err := b.source.Discard(); err != nil {
		return nil, err
	}
	b.source = nil
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.ready.Store(true)
	return b, nil
}

func (b *Baseline) persistEvidence(ctx context.Context, cut [32]byte) error {
	encoded, err := json.Marshal(b.evidence)
	if err != nil {
		return err
	}
	_, err = b.store.Update(ctx, cut, func(tx *authenticatedstate.Transaction) error {
		return tx.Put(baselineEvidenceKey, encoded)
	})
	return err
}

func baselineScope(o BaselineOptions) [32]byte {
	b := append([]byte("halro:metadata:all-rows:codec1\x00"), o.Identity[:]...)
	b = binary.BigEndian.AppendUint64(b, o.KeyGeneration)
	b = binary.BigEndian.AppendUint16(b, uint16(len(o.InstanceID)))
	b = append(b, o.InstanceID...)
	catalog := boltstore.MetadataBaselineCatalogDigest()
	b = append(b, catalog[:]...)
	return sha256.Sum256(b)
}
func baselineCut(scope [32]byte, evidence BaselineEvidence) ([32]byte, error) {
	raw, err := json.Marshal(evidence)
	if err != nil {
		return [32]byte{}, err
	}
	b := append([]byte("halro:metadata:backup-baseline-cut:v1\x00"), scope[:]...)
	b = append(b, raw...)
	return sha256.Sum256(b), nil
}

func (b *Baseline) Evidence() BaselineEvidence {
	if b == nil {
		return BaselineEvidence{}
	}
	return b.evidence
}

// Snapshot exposes only authenticated immutable reads, including absence and
// complete ranges. This read handle grants no runtime selection or role authority.
func (b *Baseline) Snapshot(ctx context.Context) (*authenticatedstate.Snapshot, error) {
	if b == nil || !b.ready.Load() || b.haPeerTransition != nil {
		return nil, authenticatedstate.ErrClosed
	}
	if b.selectedStartup {
		if !b.journal.SelectedStartupStructuralReady() {
			return nil, metadatajournal.ErrStartupAuditPending
		}
		if err := b.journal.HealthError(); err != nil {
			return nil, err
		}
	}
	return b.store.Snapshot(ctx)
}
func (b *Baseline) Close() error {
	return b.CloseContext(context.Background())
}

// CloseContext borrows only the maintenance caller's original live completion
// workspace for final qualification reads. It always joins actual owners;
// cancellation or unknown retirement never grants a refund.
func (b *Baseline) CloseContext(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.cancel()
	b.joinHAPhysicalReceives()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.borrowed && b.borrowedClosed {
		return b.closeErr
	}
	if b.borrowed {
		b.borrowedClean = b.ready.Load() || b.rotationSource
		if b.selectedStartup {
			b.borrowedClean = b.borrowedClean && b.journal.SelectedStartupStructuralReady() && b.journal.HealthError() == nil
		}
	}
	b.ready.Store(false)
	err := b.retirementErr
	if b.physicalEnabled {
		qualificationErr := b.physicalCommonSelectionLocked(ctx)
		if b.physicalDebt != 0 || b.physicalRecovery != nil || qualificationErr != nil {
			// Only the actual NoDebt/common publication maintenance can unlink
			// these original targets; ordinary Close joins FDs and keeps debt.
			b.journal.HoldPhysicalForRecovery()
			err = errors.Join(err, qualificationErr, b.store.HoldPhysicalPreparationForRecovery(ctx))
		} else {
			for _, w := range b.physicalWindows {
				if w != nil {
					err = errors.Join(err, w.closeLocked())
				}
			}
			if err == nil {
				err = b.store.RetirePhysicalPreparation(ctx)
			}
			if err != nil {
				b.retirementErr = errors.Join(b.retirementErr, ownedfile.ErrRetirement, err)
				err = b.retirementErr
				b.journal.HoldPhysicalForRecovery()
				err = errors.Join(err, b.store.HoldPhysicalPreparationForRecovery(ctx))
			}
		}
	}
	if b.journal != nil {
		err = errors.Join(err, b.journal.Close())
	}
	if b.startupStop != nil {
		b.startupStop()
	}
	if b.startupCancel != nil {
		b.startupCancel()
	}
	if b.store != nil {
		err = errors.Join(err, b.store.Close())
	}
	if b.readSource != nil {
		err = errors.Join(err, b.readSource.Close())
	}
	if b.readCloneParent != "" && err == nil {
		err = b.removeSelectedReadRoot()
	}
	if b.borrowed && b.journalCharged && !errors.Is(err, ownedfile.ErrRetirement) {
		err = errors.Join(err, b.journalDisk.Release())
		b.journalCharged = false
	}
	err = errors.Join(err, b.source.Close())
	if b.borrowed {
		b.borrowedClosed = true
		if b.physicalEnabled && err != nil {
			err = &PhysicalRetirementFailure{Owner: b, Cause: err}
		}
		b.closeErr = err
		b.borrowedClean = b.borrowedClean && err == nil
	}
	return err
}
func (b *Baseline) Discard() error {
	if b == nil {
		return nil
	}
	if b.borrowed {
		return ErrSource
	}
	b.cancel()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ready.Store(false)
	if b.retirementErr != nil {
		return b.retirementErr
	}
	// Do not remove the only root while its journal still exists or has an
	// unresolved physical deletion. Foreign journal replacements are preserved.
	if b.journal != nil {
		if err := b.journal.Discard(); err != nil {
			return err
		}
	}
	if b.journalCharged {
		if err := b.journalDisk.Release(); err != nil {
			return err
		}
		b.journalCharged = false
	}
	var err error
	if b.store != nil {
		err = b.store.Discard()
	}
	return errors.Join(err, b.source.Discard())
}
