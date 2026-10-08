package governance

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"syscall"
)

const (
	AuthenticatedGovernanceProjectionSealIndexID uint16 = 2
	AuthenticatedGovernanceProjectionSealCodec   uint16 = 1 // full fresh bbolt image, chunked in the authenticated namespace
	governanceSealChunkBytes                            = 64 << 10
	governanceSealWorkspaceRAMBytes                     = 32 << 20
)

// AuthenticatedGovernanceOptions builds a new reducer only from the selected
// native journal cut. ProjectionReservation covers the complete fresh bbolt
// ceiling in the same shared manager and remains held through inode removal.
type AuthenticatedGovernanceOptions struct {
	Native                    *Log
	Identity                  [32]byte
	RootKey                   []byte
	ProjectionPath            string
	RootDirectory             string
	ProjectionMaxDiskBytes    int64
	RootMaxDiskBytes          int64
	RootMaxAppendBytes        int64
	ProjectionDiskReservation resourcebudget.Permit
	Budget                    *resourcebudget.Manager
}

// AuthenticatedGovernanceProjection owns a newly built reducer image and its
// two roots: index 1 for native Outcome facts, index 2 for every byte of the
// derived bbolt projection. It is not a Runtime reopen or role permission.
type AuthenticatedGovernanceProjection struct {
	retirementErr  error
	retiredImage   *os.File
	retiredScratch *GovernanceProjection

	store      *authenticatedstate.Store
	Journal    GovernanceSealReference
	Projection AuthenticatedGovernanceProjectionReference
}

type AuthenticatedGovernanceProjectionReference struct {
	Schema   authenticatedstate.SealRootSchema
	Root     authenticatedstate.SealRoot
	Scope    [32]byte
	Position authenticatedstate.SealPosition
}

// OpenAuthenticatedGovernanceProjection reopens the saved projection root only
// when the caller supplies the exact native journal receipt and the previously
// selected projection reference. The caller remains responsible for deriving
// those values from independently authenticated startup authority and for
// holding the Runtime data-directory lock. OpenStore checks the selected root
// before it repairs an orphan tail or removes pending state.
func OpenAuthenticatedGovernanceProjection(
	ctx context.Context,
	directory string,
	identity [32]byte,
	rootKey []byte,
	journal GovernanceSealReference,
	selected AuthenticatedGovernanceProjectionReference,
	maxDiskBytes, maxAppendBytes int64,
	diskReservation resourcebudget.Permit,
	budget *resourcebudget.Manager,
) (*AuthenticatedGovernanceProjection, error) {
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState,
		Component: authenticatedstate.SealComponentGovernance,
		IndexID:   AuthenticatedGovernanceProjectionSealIndexID,
		Codec:     AuthenticatedGovernanceProjectionSealCodec,
	}
	if directory == "" || identity == ([32]byte{}) || len(rootKey) != sha256.Size || budget == nil ||
		maxDiskBytes <= 0 || maxAppendBytes <= 0 ||
		journal.Schema.Component != authenticatedstate.SealComponentGovernance ||
		journal.Root.Component != schema.Component || journal.Root.IndexID != AuthenticatedGovernanceSealIndexID ||
		journal.Root.Codec != AuthenticatedGovernanceSealCodec || journal.Position.Component != schema.Component ||
		selected.Schema != schema || selected.Position != journal.Position ||
		selected.Root.Component != schema.Component || selected.Root.IndexID != schema.IndexID || selected.Root.Codec != schema.Codec {
		return nil, authenticatedstate.ErrRecovery
	}
	scope := governanceProjectionScope(identity, journal)
	cut := governanceProjectionCut(scope, journal)
	if selected.Scope != scope {
		return nil, authenticatedstate.ErrRecovery
	}
	derivedKey, err := hkdf.Key(sha256.New, rootKey, scope[:], "halro:governance:authenticated-projection:v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	defer clear(derivedKey)
	expectedRoot := selected.Root
	expectedSchema := selected.Schema
	store, err := authenticatedstate.OpenStore(ctx, directory, authenticatedstate.StoreOptions{
		Scope: scope, ExpectedCut: cut, Key: derivedKey,
		MaxDiskBytes: maxDiskBytes, MaxAppendBytes: maxAppendBytes,
		Budget: budget, DiskReservation: diskReservation,
		ExpectedSealRoot: &expectedRoot, ExpectedSealSchema: &expectedSchema,
	})
	if err != nil {
		return nil, err
	}
	return &AuthenticatedGovernanceProjection{store: store, Journal: journal, Projection: selected}, nil
}

func governanceProjectionScope(identity [32]byte, journal GovernanceSealReference) [32]byte {
	h := sha256.New()
	h.Write([]byte("halro:governance:authenticated-projection-scope:v1\x00"))
	h.Write(identity[:])
	h.Write(journal.Root.FileID[:])
	h.Write(journal.Root.Digest[:])
	var b [8]byte
	for _, n := range []uint64{journal.Position.Sequence, journal.Position.Offset} {
		binary.BigEndian.PutUint64(b[:], n)
		h.Write(b[:])
	}
	h.Write(journal.Position.Hash[:])
	var scope [32]byte
	copy(scope[:], h.Sum(nil))
	return scope
}

func governanceProjectionCut(scope [32]byte, journal GovernanceSealReference) [32]byte {
	h := sha256.New()
	h.Write([]byte("halro:governance:authenticated-projection-cut:v1\x00"))
	h.Write(scope[:])
	h.Write(journal.Root.Digest[:])
	h.Write(journal.Position.Hash[:])
	var b [8]byte
	for _, n := range []uint64{journal.Position.Sequence, journal.Position.Offset} {
		binary.BigEndian.PutUint64(b[:], n)
		h.Write(b[:])
	}
	var cut [32]byte
	copy(cut[:], h.Sum(nil))
	return cut
}

// BuildAuthenticatedGovernanceProjection creates both the native journal
// prefix root and a full content root for the fresh Governance reducer at that
// exact Summary. It never imports an existing ordinary projection. The root
// builder authenticates the complete source during rebuild, verifies the
// reducer head, closes it, then streams the owned image into authenticatedstate.
func BuildAuthenticatedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions) (*AuthenticatedGovernanceProjection, error) {
	return buildAuthenticatedGovernanceProjection(ctx, o, nil)
}

func buildAuthenticatedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, closeImage func(*os.File) error) (result *AuthenticatedGovernanceProjection, resultErr error) {
	var retiredImage *os.File
	var scratch *GovernanceProjection
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			if result == nil {
				result = &AuthenticatedGovernanceProjection{}
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
	if o.Native == nil || o.Identity == ([32]byte{}) || len(o.RootKey) != sha256.Size || o.Budget == nil ||
		o.ProjectionPath == "" || o.RootDirectory == "" || o.ProjectionMaxDiskBytes <= 0 || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 {
		return nil, errors.New("authenticated Governance build requires native, identity, root key, paths, budget, and finite limits")
	}
	o.Native.mu.Lock()
	if o.Native.file == nil || o.Native.closed || o.Native.closing || o.Native.readerRetirementErr != nil || o.Native.terminalErr != nil || len(o.Native.key) != governanceHMACKeySize {
		err := errors.Join(ErrGovernanceSealSource, o.Native.terminalErr, o.Native.closeErr, o.Native.readerRetirementErr)
		o.Native.mu.Unlock()
		return nil, err
	}
	cut := Summary{Records: o.Native.sequence, Bytes: o.Native.offset, LastHash: o.Native.lastHash}
	sourcePath := o.Native.path
	governanceKey := bytes.Clone(o.Native.key)
	o.Native.mu.Unlock()
	defer clear(governanceKey)
	workspace, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes})
	if err != nil {
		return nil, fmt.Errorf("authenticated Governance bounded workspace: %w", err)
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, workspace.Release())
		}
	}()
	projectionReservation, err := o.ProjectionDiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.ProjectionMaxDiskBytes})
	if err != nil {
		return nil, fmt.Errorf("authenticated Governance projection reservation: %w", err)
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, projectionReservation.Release())
		}
	}()

	projection, err := BuildGovernanceProjectionWithLimit(ctx, sourcePath, governanceKey, o.ProjectionPath, cut, o.ProjectionMaxDiskBytes)
	if err != nil {
		return nil, err
	}
	scratch = projection
	defer func() {
		resultErr = errors.Join(resultErr, projection.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if err := projection.file.Retire(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
		}
		if resultErr != nil && result != nil {
			resultErr = errors.Join(resultErr, result.Discard())
			result = nil
		}
	}()
	projectedHead, err := projection.ReadHead(ctx)
	if err != nil || projectedHead != cut {
		return nil, errors.Join(ErrCorrupt, err)
	}
	journal, err := o.Native.SealReference(ctx, o.Budget, cut)
	if err != nil {
		return nil, errors.Join(ErrGovernanceSealSource, err)
	}
	if err := projection.Close(); err != nil {
		return nil, err
	}
	image, err := os.OpenFile(o.ProjectionPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !imageClosed {
			resultErr = errors.Join(resultErr, closeOwnedImage(image))
		}
	}()
	if err := projection.file.VerifyFile(image); err != nil {
		return nil, errors.Join(err, closeOwnedImage(image))
	}
	info, err := image.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > o.ProjectionMaxDiskBytes {
		return nil, errors.Join(ErrProjectionDiskLimit, err, closeOwnedImage(image))
	}
	scope := governanceProjectionScope(o.Identity, journal)
	expectedCut := governanceProjectionCut(scope, journal)
	rootKey, err := hkdf.Key(sha256.New, o.RootKey, scope[:], "halro:governance:authenticated-projection:v1", sha256.Size)
	if err != nil {
		return nil, errors.Join(err, closeOwnedImage(image))
	}
	defer clear(rootKey)
	var offset int64
	var ordinal uint64
	var sourceErr error
	store, err := authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{
		Scope: scope, ExpectedCut: expectedCut, Key: rootKey,
		MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget,
	}, func() (authenticatedstate.Row, bool, error) {
		if offset >= info.Size() {
			return authenticatedstate.Row{}, false, nil
		}
		if err := ctx.Err(); err != nil {
			return authenticatedstate.Row{}, false, err
		}
		n := int64(governanceSealChunkBytes)
		if remaining := info.Size() - offset; remaining < n {
			n = remaining
		}
		value := make([]byte, int(n))
		if count, err := image.ReadAt(value, offset); err != nil || count != len(value) {
			err = errors.Join(io.ErrUnexpectedEOF, err)
			sourceErr = err
			return authenticatedstate.Row{}, false, err
		}
		key := make([]byte, 9)
		key[0] = 1
		binary.BigEndian.PutUint64(key[1:], ordinal)
		ordinal++
		offset += n
		return authenticatedstate.Row{Key: key, Value: value}, true, nil
	})
	finalInfo, finalStatErr := image.Stat()
	pathInfo, pathStatErr := os.Lstat(o.ProjectionPath)
	if finalStatErr != nil || pathStatErr != nil || !os.SameFile(info, finalInfo) || !os.SameFile(info, pathInfo) || finalInfo.Size() != info.Size() || finalInfo.ModTime() != info.ModTime() {
		sourceErr = errors.Join(sourceErr, errors.New("Governance projection changed while its root was built"), finalStatErr, pathStatErr)
	}
	closeErr := closeOwnedImage(image)
	resultErr = errors.Join(resultErr, closeErr)
	if err != nil || closeErr != nil || sourceErr != nil {
		if store != nil {
			resultErr = errors.Join(resultErr, store.Discard())
		}
		return nil, errors.Join(err, closeErr, sourceErr)
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentGovernance, IndexID: AuthenticatedGovernanceProjectionSealIndexID, Codec: AuthenticatedGovernanceProjectionSealCodec}
	root, err := store.SealRootReference(ctx, schema)
	if err != nil {
		resultErr = errors.Join(resultErr, store.Discard())
		return nil, err
	}
	identity, err := store.RootIdentity(ctx)
	if err != nil || identity.Scope != scope || identity.Cut != expectedCut || identity.ID != root.FileID || identity.Rows != root.Rows || identity.Revision == 0 || identity.Digest == ([32]byte{}) {
		resultErr = errors.Join(resultErr, store.Discard())
		return nil, errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	result = &AuthenticatedGovernanceProjection{
		store: store, Journal: journal,
		Projection: AuthenticatedGovernanceProjectionReference{Schema: schema, Root: root, Scope: scope, Position: journal.Position},
	}
	return result, nil
}

func (p *AuthenticatedGovernanceProjection) Close() error {
	if p != nil && p.retirementErr != nil {
		return p.retirementErr
	}
	if p == nil || p.store == nil {
		return nil
	}
	return p.store.Close()
}

// WriteProjectionImage authenticates the complete saved reducer image. The
// caller owns destination disk admission, Sync/Close and failed-output cleanup;
// this method neither trusts an existing bbolt file nor grants Runtime readiness.
func (p *AuthenticatedGovernanceProjection) WriteProjectionImage(ctx context.Context, destination io.Writer, maxBytes int64) (int64, error) {
	if p == nil || p.store == nil {
		return 0, authenticatedstate.ErrClosed
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentGovernance, IndexID: AuthenticatedGovernanceProjectionSealIndexID, Codec: AuthenticatedGovernanceProjectionSealCodec}
	return p.store.WriteImage(ctx, destination, schema, p.Projection.Root, governanceSealChunkBytes, maxBytes)
}

func (p *AuthenticatedGovernanceProjection) Discard() error {
	if p != nil && p.retirementErr != nil {
		return p.retirementErr
	}
	if p == nil || p.store == nil {
		return nil
	}
	return p.store.Discard()
}
