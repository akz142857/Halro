package usage

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"syscall"
)

const (
	AuthenticatedUsageSealIndexID uint16 = 1
	AuthenticatedUsageSealCodec   uint16 = 1 // exact fresh bbolt image, chunked in the authenticated namespace
	usageSealChunkBytes                  = 64 << 10
	usageSealWorkspaceRAMBytes           = 32 << 20
)

// AuthenticatedUsageOptions builds a disposable Usage reducer from the exact
// authenticated Ledger cut selected by Owner, then imports the closed reducer
// image into an authenticated component store. ScratchReservation must cover
// the complete scratch DB ceiling in the same resource manager; it is retained
// until the captured scratch inode has been removed.
//
// This is a component root only. It does not establish a cross-component
// common cut or select a Runtime role.
type AuthenticatedUsageOptions struct {
	PhysicalTerminalReserve bool
	Native                  *ledger.Log
	Owner                   *ledger.AuthenticatedLedgerProjection
	Identity                [32]byte
	UsageKey                []byte
	ScratchPath             string
	RootDirectory           string
	ScratchMaxDiskBytes     int64
	RootMaxDiskBytes        int64
	RootMaxAppendBytes      int64
	ScratchReservation      resourcebudget.Permit
	Budget                  *resourcebudget.Manager
}

// AuthenticatedUsageProjection owns an authenticated row store. Close
// preserves the saved files and releases live handles; Discard removes only a
// store created by this handle. Reopened handles borrow the persistent files.
type AuthenticatedUsageProjection struct {
	retirementErr  error
	retiredImage   *os.File
	retiredScratch *DiskProjection

	store     *authenticatedstate.Store
	reference AuthenticatedUsageSealReference
	source    ledger.AuthenticatedLedgerSealReference
}

type AuthenticatedUsageSealReference struct {
	Schema   authenticatedstate.SealRootSchema
	Root     authenticatedstate.SealRoot
	Scope    [32]byte
	Position authenticatedstate.SealPosition
}

// OpenAuthenticatedUsageProjection reopens only a root selected for the exact
// authenticated Ledger cut and stable Usage identity. The caller must derive
// both references from independently authenticated startup authority and hold
// the Runtime data-directory lock. The selected root is checked before any
// pending cleanup or orphan-tail repair.
func OpenAuthenticatedUsageProjection(
	ctx context.Context,
	directory string,
	identity [32]byte,
	usageKey []byte,
	ledgerRef ledger.AuthenticatedLedgerSealReference,
	selected AuthenticatedUsageSealReference,
	maxDiskBytes, maxAppendBytes int64,
	diskReservation resourcebudget.Permit,
	budget *resourcebudget.Manager,
) (*AuthenticatedUsageProjection, error) {
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState,
		Component: authenticatedstate.SealComponentUsage,
		IndexID:   AuthenticatedUsageSealIndexID,
		Codec:     AuthenticatedUsageSealCodec,
	}
	ledgerSchema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState,
		Component: authenticatedstate.SealComponentLedger,
		IndexID:   ledger.AuthenticatedLedgerSealIndexID,
		Codec:     ledger.AuthenticatedLedgerSealCodec,
	}
	if directory == "" || identity == ([32]byte{}) || len(usageKey) != sha256.Size || budget == nil ||
		maxDiskBytes <= 0 || maxAppendBytes <= 0 || ledgerRef.Schema != ledgerSchema ||
		ledgerRef.Root.Component != ledgerSchema.Component || ledgerRef.Root.IndexID != ledgerSchema.IndexID ||
		ledgerRef.Root.Codec != ledgerSchema.Codec || ledgerRef.Scope == ([32]byte{}) || ledgerRef.Anchor.Head.Offset < 0 ||
		selected.Schema != schema || selected.Root.Component != schema.Component ||
		selected.Root.IndexID != schema.IndexID || selected.Root.Codec != schema.Codec ||
		selected.Scope != authenticatedUsageScope(identity, ledgerRef) || selected.Position != authenticatedUsagePosition(ledgerRef) {
		return nil, authenticatedstate.ErrRecovery
	}
	scope := authenticatedUsageScope(identity, ledgerRef)
	cut := authenticatedUsageCut(identity, ledgerRef)
	rootKey, err := hkdf.Key(sha256.New, usageKey, scope[:], "halro:usage:authenticated-rows:v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	defer clear(rootKey)
	expectedRoot := selected.Root
	expectedSchema := selected.Schema
	store, err := authenticatedstate.OpenStore(ctx, directory, authenticatedstate.StoreOptions{
		Scope: scope, ExpectedCut: cut, Key: rootKey,
		MaxDiskBytes: maxDiskBytes, MaxAppendBytes: maxAppendBytes,
		Budget: budget, DiskReservation: diskReservation,
		ExpectedSealRoot: &expectedRoot, ExpectedSealSchema: &expectedSchema,
	})
	if err != nil {
		return nil, err
	}
	return &AuthenticatedUsageProjection{store: store, reference: selected, source: ledgerRef}, nil
}

func authenticatedUsagePosition(source ledger.AuthenticatedLedgerSealReference) authenticatedstate.SealPosition {
	return authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentUsage, Epoch: uint64(source.Anchor.Epoch),
		Generation: source.Anchor.Head.Generation, Sequence: source.Anchor.Head.Sequence,
		Offset: uint64(source.Anchor.Head.Offset), Hash: source.Anchor.Hash}
}

func authenticatedUsageCut(identity [32]byte, source ledger.AuthenticatedLedgerSealReference) [32]byte {
	h := sha256.New()
	h.Write([]byte("halro:usage:authenticated-cut:v1\x00"))
	h.Write(identity[:])
	h.Write(source.Scope[:])
	h.Write(source.Root.Digest[:])
	var b [8]byte
	for _, v := range []uint64{source.Anchor.Head.Generation, source.Anchor.Head.Sequence, uint64(source.Anchor.Head.Offset), uint64(source.Anchor.Epoch)} {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	h.Write(source.Anchor.Hash[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func authenticatedUsageScope(identity [32]byte, source ledger.AuthenticatedLedgerSealReference) [32]byte {
	h := sha256.New()
	h.Write([]byte("halro:usage:authenticated-scope:v1\x00"))
	h.Write(identity[:])
	h.Write(source.Scope[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

// BuildAuthenticatedUsageProjection reconstructs all Usage rows through the
// authenticated Ledger owner's exact applied cut. Native replay still checks
// the complete durable suffix, but records past the selected cut are excluded.
// The returned root covers every byte of the newly built, closed bbolt image;
// the ordinary existing Usage DB is never imported or trusted.
func BuildAuthenticatedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions) (*AuthenticatedUsageProjection, error) {
	return buildAuthenticatedUsageProjection(ctx, o, nil)
}

func buildAuthenticatedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, closeImage func(*os.File) error) (result *AuthenticatedUsageProjection, resultErr error) {
	var retiredImage *os.File
	var scratch *DiskProjection
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			if result == nil {
				result = &AuthenticatedUsageProjection{}
			}
			result.retirementErr = resultErr
			result.retiredImage = retiredImage
			result.retiredScratch = scratch
		}
	}()
	imageClosed := false
	var imageCloseErr error
	closeOwnedImage := func(file *os.File) error {
		if imageClosed {
			return imageCloseErr
		}
		imageClosed = true
		closeFile := closeImage
		if closeFile == nil {
			closeFile = (*os.File).Close
		}
		if err := closeFile(file); err != nil {
			retiredImage = file
			imageCloseErr = ownedfile.CloseError(file, err)
			return imageCloseErr
		}
		return nil
	}
	if o.Native == nil || o.Owner == nil || o.Identity == ([32]byte{}) || len(o.UsageKey) != 32 ||
		o.Budget == nil || o.ScratchMaxDiskBytes <= 0 || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 ||
		o.ScratchPath == "" || o.RootDirectory == "" {
		return nil, errors.New("authenticated Usage build requires native, selected owner, identity, keys, paths, and finite limits")
	}
	workspace, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
	if err != nil {
		return nil, fmt.Errorf("authenticated Usage bounded workspace: %w", err)
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, workspace.Release())
		}
	}()
	reservation, err := o.ScratchReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.ScratchMaxDiskBytes})
	if err != nil {
		return nil, fmt.Errorf("authenticated Usage scratch reservation: %w", err)
	}
	reservationHeld := true
	defer func() {
		if reservationHeld && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, reservation.Release())
		}
	}()

	ledgerRef, err := o.Owner.SealReference(ctx)
	if err != nil {
		return nil, err
	}
	if ledgerRef.Anchor.Head.Offset < 0 || ledgerRef.Scope == ([32]byte{}) {
		return nil, ledger.ErrProjectionCorrupt
	}
	derivative, disk, err := NewDiskLedgerDerivativeWithLimit(o.Owner, o.ScratchPath, o.ScratchMaxDiskBytes)
	if err != nil {
		return nil, err
	}
	scratch = disk
	defer func() {
		resultErr = errors.Join(resultErr, derivativeClose(derivative), disk.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if err := disk.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
		}
		if resultErr != nil && result != nil {
			resultErr = errors.Join(resultErr, result.Discard())
			result = nil
		}
	}()

	var batch []ledger.Record
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := derivative.ApplyReplayBatchContext(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		batchBytes = 0
		return nil
	}
	seenCut := ledgerRef.Anchor.Head.Sequence == 0 && ledgerRef.Anchor == (ledger.ReplayAnchor{})
	_, err = o.Native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(record ledger.Record) error {
		if record.Sequence > ledgerRef.Anchor.Head.Sequence {
			return nil // still authenticated by ReplayAuthenticated; outside the selected projection cut
		}
		if record.Sequence == ledgerRef.Anchor.Head.Sequence {
			if (ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}) != ledgerRef.Anchor.Head || record.Hash != ledgerRef.Anchor.Hash || record.Epoch != ledgerRef.Anchor.Epoch {
				return ledger.ErrProjectionCorrupt
			}
			seenCut = true
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(encoded) > diskReplayBatchBytes {
			return ErrDiskUsageLimit
		}
		if len(batch) == diskReplayBatchRows || len(encoded) > diskReplayBatchBytes-batchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, record)
		batchBytes += len(encoded)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !seenCut {
		return nil, ledger.ErrProjectionCorrupt
	}
	if err := flush(); err != nil {
		return nil, err
	}
	usageAnchor, err := disk.ReadReplayAnchor(ctx)
	if err != nil || usageAnchor != ledgerRef.Anchor {
		return nil, errors.Join(ledger.ErrProjectionCorrupt, err)
	}
	currentLedger, err := o.Owner.SealReference(ctx)
	if err != nil || currentLedger != ledgerRef {
		return nil, errors.Join(ledger.ErrProjectionUnavailable, err)
	}

	if err := disk.Close(); err != nil {
		return nil, err
	}
	image, err := os.OpenFile(o.ScratchPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !imageClosed {
			resultErr = errors.Join(resultErr, closeOwnedImage(image))
		}
	}()
	if err := disk.file.VerifyFile(image); err != nil {
		return nil, errors.Join(err, closeOwnedImage(image))
	}
	info, err := image.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > o.ScratchMaxDiskBytes {
		return nil, errors.Join(ErrDiskUsageDiskLimit, err, closeOwnedImage(image))
	}
	scope := authenticatedUsageScope(o.Identity, ledgerRef)
	cut := authenticatedUsageCut(o.Identity, ledgerRef)
	rootKey, err := hkdf.Key(sha256.New, o.UsageKey, scope[:], "halro:usage:authenticated-rows:v1", 32)
	if err != nil {
		return nil, errors.Join(err, closeOwnedImage(image))
	}
	defer clear(rootKey)
	var offset int64
	var ordinal uint64
	var sourceErr error
	store, err := authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{
		Scope: scope, ExpectedCut: cut, Key: rootKey,
		MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget,
	}, func() (authenticatedstate.Row, bool, error) {
		if offset >= info.Size() {
			return authenticatedstate.Row{}, false, nil
		}
		if err := ctx.Err(); err != nil {
			return authenticatedstate.Row{}, false, err
		}
		n := int64(usageSealChunkBytes)
		if remaining := info.Size() - offset; remaining < n {
			n = remaining
		}
		value := make([]byte, int(n))
		count, readErr := image.ReadAt(value, offset)
		if readErr != nil || count != len(value) {
			readErr = errors.Join(io.ErrUnexpectedEOF, readErr)
			sourceErr = readErr
			return authenticatedstate.Row{}, false, readErr
		}
		key := make([]byte, 9)
		key[0] = 1
		binary.BigEndian.PutUint64(key[1:], ordinal)
		ordinal++
		offset += n
		return authenticatedstate.Row{Key: key, Value: value}, true, nil
	})
	finalInfo, finalStatErr := image.Stat()
	pathInfo, pathStatErr := os.Lstat(o.ScratchPath)
	if finalStatErr != nil || pathStatErr != nil || !os.SameFile(info, finalInfo) || !os.SameFile(info, pathInfo) ||
		finalInfo.Size() != info.Size() || finalInfo.ModTime() != info.ModTime() {
		sourceErr = errors.Join(sourceErr, errors.New("Usage scratch image changed while it was authenticated"), finalStatErr, pathStatErr)
	}
	closeErr := closeOwnedImage(image)
	resultErr = errors.Join(resultErr, closeErr)
	if err != nil || closeErr != nil || sourceErr != nil {
		if store != nil {
			resultErr = errors.Join(resultErr, store.Discard())
		}
		return nil, errors.Join(err, closeErr, sourceErr)
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentUsage, IndexID: AuthenticatedUsageSealIndexID, Codec: AuthenticatedUsageSealCodec}
	root, err := store.SealRootReference(ctx, schema)
	if err != nil {
		resultErr = errors.Join(resultErr, store.Discard())
		return nil, err
	}
	identity, err := store.RootIdentity(ctx)
	if err != nil || identity.Scope != scope || identity.Cut != cut || identity.ID != root.FileID || identity.Rows != root.Rows || identity.Revision == 0 || identity.Digest == ([32]byte{}) {
		resultErr = errors.Join(resultErr, store.Discard())
		return nil, errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	position := authenticatedUsagePosition(ledgerRef)
	result = &AuthenticatedUsageProjection{store: store, source: ledgerRef, reference: AuthenticatedUsageSealReference{Schema: schema, Root: root, Scope: scope, Position: position}}
	return result, nil
}

func derivativeClose(d *LedgerDerivative) error {
	if d != nil {
		d.Close()
	}
	return nil
}

func (p *AuthenticatedUsageProjection) SealReference() AuthenticatedUsageSealReference {
	if p == nil {
		return AuthenticatedUsageSealReference{}
	}
	return p.reference
}

// WriteProjectionImage authenticates every saved byte before forwarding it to
// an exclusive, caller-budgeted destination. It does not reopen an arbitrary
// existing Usage DB or establish current role/native suffix authority.
func (p *AuthenticatedUsageProjection) WriteProjectionImage(ctx context.Context, destination io.Writer, maxBytes int64) (int64, error) {
	if p == nil || p.store == nil {
		return 0, authenticatedstate.ErrClosed
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentUsage, IndexID: AuthenticatedUsageSealIndexID, Codec: AuthenticatedUsageSealCodec}
	return p.store.WriteImage(ctx, destination, schema, p.reference.Root, usageSealChunkBytes, maxBytes)
}

func (p *AuthenticatedUsageProjection) Close() error {
	if p != nil && p.retirementErr != nil {
		return p.retirementErr
	}
	if p == nil || p.store == nil {
		return nil
	}
	return p.store.Close()
}

func (p *AuthenticatedUsageProjection) Discard() error {
	if p != nil && p.retirementErr != nil {
		return p.retirementErr
	}
	if p == nil || p.store == nil {
		return nil
	}
	return p.store.Discard()
}
