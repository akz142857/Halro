package ledger

// This is a private maintenance candidate for D-03, not a Runtime checkpoint.
// Its initial rows come only from canonical genesis and authenticated native
// facts. No Bolt file is imported or reopened as an authority.

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type AuthenticatedLedgerOptions struct {
	// Identity is an independently established Ledger/component identity. It
	// does not grant a role, term, schema or permission to resume after restart.
	Identity                     [32]byte
	MaxDiskBytes, MaxAppendBytes int64
	Budget                       *resourcebudget.Manager
	// Nil selects the complete frozen head. A non-nil prefix still requires
	// full source authentication, including the excluded durable suffix.
	Prefix *ProjectionPrefix
}

const (
	AuthenticatedLedgerSealIndexID uint16 = 1
	AuthenticatedLedgerSealCodec   uint16 = 1 // complete ordinal-keyed projection namespace
)

// AuthenticatedLedgerSealReference pairs the complete ordinal-keyed Ledger
// namespace root with the exact native position committed inside that root.
// It is a component reference only; it does not establish a cross-store cut,
// startup role eligibility, or Runtime readiness.
type AuthenticatedLedgerSealReference struct {
	Schema authenticatedstate.SealRootSchema
	Root   authenticatedstate.SealRoot
	Scope  [32]byte
	Anchor ReplayAnchor
}

type AuthenticatedLedgerProjection struct {
	physicalReplica      *TerminalPhysicalWindow // protected by original native.mu
	physicalRecovery     *TerminalPhysicalWindow
	physicalRecoveryWork *resourcebudget.CompletableWork
	businessReads        atomic.Bool
	businessReadOnce     sync.Once
	businessReadLane     chan struct{}
	businessReadWaiters  atomic.Int32
	selectedNative       atomic.Pointer[Log]
	sealActive           atomic.Bool
	sealDebt             resourcebudget.Permit
	sealErr              error
	mu                   sync.RWMutex
	store                *authenticatedstate.Store
	pack                 projectionPackObservation
	ready                bool
	closeComplete        bool
	closeErr             error
	readFailure          atomic.Pointer[authenticatedLedgerReadFault] // first fault retires concurrent readers too
	durableApply         atomic.Bool                                  // one accepted-fact update may wait for existing readers
	durableMu            sync.Mutex
	durableDone          chan struct{} // Close joins the admitted durable caller including its fee
	scope                [32]byte
	budget               *resourcebudget.Manager
	nativePath           string
	nativeKeyTag         [32]byte
	failure              error
	closeContext         context.Context
	closeCancel          context.CancelFunc
	recoveryDone         chan struct{} // recovery callback joined by Close; guarded by mu
	cohortDone           chan struct{} // visitor lifetime fee; may itself call Close
}

// The native decoder and source-validation scratch remain charged while an
// update owns its separate bounded working set. Physical calibration is pending.
const AuthenticatedLedgerSourceRAMBytes int64 = 32 << 20

// Ordinals are part of this codec version, not derived from an ordinary DB.
// The scope binds the complete bucket catalog and shared reducer version.
func authenticatedLedgerBucket(name string) (byte, error) {
	for i, candidate := range projectionBuckets {
		if candidate == name {
			return byte(i + 1), nil
		}
	}
	for i, candidate := range projectionListBuckets {
		if candidate == name {
			return byte(len(projectionBuckets) + i + 1), nil
		}
	}
	return 0, ErrProjectionCorrupt
}
func authenticatedLedgerKey(bucket string, key []byte) ([]byte, error) {
	ordinal, err := authenticatedLedgerBucket(bucket)
	if err != nil {
		return nil, err
	}
	if len(key) == 0 || len(key)+1 > authenticatedstate.MaxKeyBytes {
		return nil, ErrProjectionPageLimit
	}
	return append([]byte{ordinal}, key...), nil
}
func authenticatedLedgerScope(identity [32]byte) [32]byte {
	b := append([]byte("halro:ledger:authenticated-rows:codec1:apply2\x00"), identity[:]...)
	for _, list := range [][]string{projectionBuckets, projectionListBuckets} {
		for _, name := range list {
			b = binary.BigEndian.AppendUint16(b, uint16(len(name)))
			b = append(b, name...)
		}
	}
	return sha256.Sum256(b)
}
func authenticatedLedgerCut(scope [32]byte, anchor ReplayAnchor) [32]byte {
	b := append([]byte("halro:ledger:authenticated-rows:native-cut:v1\x00"), scope[:]...)
	b = binary.BigEndian.AppendUint64(b, anchor.Head.Generation)
	b = binary.BigEndian.AppendUint64(b, uint64(anchor.Head.Offset))
	b = binary.BigEndian.AppendUint64(b, anchor.Head.Sequence)
	b = append(b, anchor.Hash[:]...)
	b = append(b, anchor.Epoch)
	return sha256.Sum256(b)
}

type authenticatedProjectionRows struct {
	tx *authenticatedstate.Transaction
}

func (a authenticatedProjectionRows) get(bucket string, key []byte) ([]byte, bool, error) {
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return nil, false, err
	}
	return a.tx.Get(k)
}
func (a authenticatedProjectionRows) put(bucket string, key, value []byte) error {
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return err
	}
	return a.tx.Put(k, value)
}
func (a authenticatedProjectionRows) delete(bucket string, key []byte) error {
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return err
	}
	return a.tx.Delete(k)
}
func (a authenticatedProjectionRows) floor(bucket string, key []byte) ([]byte, []byte, bool, error) {
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return nil, nil, false, err
	}
	row, found, err := a.tx.Floor(k)
	if err != nil || !found {
		return nil, nil, false, err
	}
	if len(row.Key) < 2 || row.Key[0] != k[0] {
		return nil, nil, false, nil
	}
	return row.Key[1:], row.Value, true, nil
}

// BuildAuthenticatedLedgerProjection requires exclusive, quiescent native
// maintenance ownership and an independently verified expected source report.
// Every native fact atomically updates all domain rows and indexes. Intermediate
// roots stay in the unpublished fresh directory; ready is set only after the
// entire frozen source, allowed cut and final physical head have been checked.
// This constructor imports no saved DB. The separate component opener requires
// authenticated saved contents, independent native/role cuts and disk ownership.
func BuildAuthenticatedLedgerProjection(ctx context.Context, source string, ledgerKey []byte, output string, expected ChainReport, o AuthenticatedLedgerOptions) (result *AuthenticatedLedgerProjection, resultErr error) {
	return buildAuthenticatedLedgerProjection(ctx, source, ledgerKey, output, expected, o, ownedfile.CloseFile)
}

func buildAuthenticatedLedgerProjection(ctx context.Context, source string, ledgerKey []byte, output string, expected ChainReport, o AuthenticatedLedgerOptions, closeActive func(*os.File) error) (result *AuthenticatedLedgerProjection, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ledgerKey) != 32 || o.Identity == ([32]byte{}) || o.Budget == nil {
		return nil, ErrProjectionSource
	}
	// Epoch 1-3 facts have only CRC evidence; a later MAC frame does not bind
	// their earlier contents. They need the still-unimplemented independently
	// accepted legacy baseline, not an automatic upgrade by this root signer.
	if expected.ChecksumOnly != 0 {
		return nil, fmt.Errorf("%w: checksum-era history requires an independently accepted legacy baseline", ErrProjectionSource)
	}
	nativePath, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	var prefix *ProjectionPrefix
	if o.Prefix != nil {
		v := *o.Prefix
		prefix = &v
		if v.Head.Offset < 0 || v.Head.Generation == 0 && v != (ProjectionPrefix{}) {
			return nil, ErrProjectionCorrupt
		}
	}
	scope := authenticatedLedgerScope(o.Identity)
	key, err := hkdf.Key(sha256.New, ledgerKey, scope[:], "halro:ledger:authenticated-rows:hmac:v1", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	sourcePermit, err := o.Budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = finishAuthenticatedSourceCredit(sourcePermit, resultErr)
		var debt *authenticatedSourceDebt
		if errors.As(resultErr, &debt) {
			if result != nil {
				resultErr = errors.Join(resultErr, result.Discard())
				result = nil
			}
		}
	}()
	// Only these four canonical constants seed the fresh component. Fixed-size
	// initialization order is the actual tree-key order, not a history sort.
	names := []string{"export_run_count", "export_work_unit_count", "pending_count", "position"}
	values := []any{uint64(0), uint64(0), int64(0), projectionPosition{}}
	i := 0
	store, err := authenticatedstate.CreateStore(ctx, output, authenticatedstate.StoreOptions{
		Scope: scope, ExpectedCut: authenticatedLedgerCut(scope, ReplayAnchor{}), Key: key,
		MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, Budget: o.Budget,
	}, func() (authenticatedstate.Row, bool, error) {
		if i == len(names) {
			return authenticatedstate.Row{}, false, nil
		}
		k, err := authenticatedLedgerKey("meta", []byte(names[i]))
		if err != nil {
			return authenticatedstate.Row{}, false, err
		}
		v, err := json.Marshal(values[i])
		i++
		return authenticatedstate.Row{Key: k, Value: v}, err == nil, err
	})
	if err != nil {
		return nil, err
	}
	p := &AuthenticatedLedgerProjection{store: store, scope: scope, budget: o.Budget, nativePath: nativePath, nativeKeyTag: sha256.Sum256(ledgerKey)}
	p.closeContext, p.closeCancel = context.WithCancel(context.Background())
	failed := true
	defer func() {
		if failed {
			resultErr = errors.Join(resultErr, p.Discard())
		}
	}()
	seenPrefix := prefix != nil && *prefix == (ProjectionPrefix{})
	var prefixMismatch error
	checkPrefix := func(head Watermark, hash [32]byte) error {
		if prefix != nil && head == prefix.Head {
			seenPrefix = true
			if hash != prefix.Hash {
				prefixMismatch = fmt.Errorf("%w: permitted prefix hash differs", ErrProjectionCorrupt)
			}
		}
		return nil
	}
	var current ReplayAnchor
	report, partial, err := inspectReplayAuthenticatedWithClose(ctx, source, ledgerKey, func(r Record) error {
		if err := checkPrefix(Watermark{r.Generation, r.Offset, r.Sequence}, r.Hash); err != nil {
			return err
		}
		if prefix != nil && (r.Generation > prefix.Head.Generation || r.Generation == prefix.Head.Generation && r.Offset > prefix.Head.Offset) {
			return nil
		}
		target := ReplayAnchor{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash, Epoch: r.Epoch}
		_, err := store.Update(ctx, authenticatedLedgerCut(scope, target), func(tx *authenticatedstate.Transaction) error {
			return applyProjectionRecord(ctx, authenticatedProjectionRows{tx}, r)
		})
		if err == nil {
			current = target
		} else {
			err = fmt.Errorf("native record %d (kind %d): %w", r.Sequence, r.Event.Kind, err)
		}
		return err
	}, checkPrefix, closeActive)
	if err != nil {
		if !isCallerVisitError(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.Join(ErrProjectionSource, err)
		}
		return nil, err
	}
	if partial || report != expected {
		return nil, errors.Join(ErrProjectionSource, ErrProjectionCorrupt)
	}
	if prefixMismatch != nil {
		return nil, prefixMismatch
	}
	if prefix != nil && !seenPrefix {
		return nil, fmt.Errorf("%w: permitted prefix is not a native boundary", ErrProjectionCorrupt)
	}
	target := ReplayAnchor{Head: report.Head, Hash: report.ChainHash, Epoch: current.Epoch}
	if prefix != nil {
		target.Head, target.Hash = prefix.Head, prefix.Hash
	}
	if target != current {
		_, err = store.Update(ctx, authenticatedLedgerCut(scope, target), func(tx *authenticatedstate.Transaction) error {
			return projectionRowsPut(authenticatedProjectionRows{tx}, "meta", []byte("position"), projectionPosition{target.Head, target.Hash, target.Epoch})
		})
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.ready, failed = true, false
	return p, nil
}

func (p *AuthenticatedLedgerProjection) Close() error {
	return p.CloseContext(context.Background())
}

// CloseContext joins the original admitted workers and descriptor exits. The
// context may lend their preowned completion workspace for current-seal checks.
func (p *AuthenticatedLedgerProjection) CloseContext(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.closeCancel()
	p.joinDurableApply()
	p.joinRecovery()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closeComplete {
		return p.closeErr
	}
	var physicalErr error
	if native := p.selectedNative.Load(); native != nil {
		physicalErr = native.holdUnselectedPhysicalProjectionLocked(ctx, p)
	}
	p.ready = false
	storeErr := p.store.Close()
	// Recovery has joined above. Unspent signed physical candidates remain
	// Store-owned obligations; withdrawing the RAM workspace cannot delete
	// them or forgive their native debt.
	if p.physicalRecoveryWork != nil {
		workErr := p.physicalRecoveryWork.Close()
		if workErr == nil {
			workErr = p.physicalRecoveryWork.Active.Release()
		}
		if workErr == nil {
			p.physicalRecoveryWork = nil
		} else {
			p.sealErr = errors.Join(p.sealErr, workErr)
		}
	}
	p.closeErr = errors.Join(physicalErr, storeErr, p.sealRetirementError())
	p.closeComplete = true
	return p.closeErr
}
func (p *AuthenticatedLedgerProjection) Discard() error {
	if p == nil {
		return nil
	}
	p.closeCancel()
	p.joinDurableApply()
	p.joinRecovery()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready = false
	if err := p.sealRetirementError(); err != nil {
		return err
	}
	return p.store.Discard()
}

// Called with mu held; uncertain common publication cannot retire its roots.
func (p *AuthenticatedLedgerProjection) sealRetirementError() error { return p.sealErr }

// PackUsage reports the page pack's use; see authenticatedstate.Store.
func (p *AuthenticatedLedgerProjection) PackUsage() (used, usable uint64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	return p.store.PackUsage()
}

// projectionPackObservation keeps the page pack's last observed use and the
// largest bytes one applied record has appended, for admission that must not
// wait on the store while an Apply holds it.
type projectionPackObservation struct {
	used, usable, recordBytes atomic.Uint64
}

func (o *projectionPackObservation) updated(store *authenticatedstate.Store, appended int64) {
	for per := uint64(max(appended, 0)); ; {
		old := o.recordBytes.Load()
		if per <= old || o.recordBytes.CompareAndSwap(old, per) {
			break
		}
	}
	if used, usable, ok := store.PackUsage(); ok {
		o.used.Store(used)
		o.usable.Store(usable)
	}
}

// PackAdmission reports the page pack's use, refreshed when the store is not
// busy and otherwise as last observed, and the largest bytes one applied
// record has appended. Every applied record is its own transaction. Zero
// recordBytes means none has been observed yet. It never waits.
func (p *AuthenticatedLedgerProjection) PackAdmission() (used, usable, recordBytes uint64) {
	if p == nil {
		return 0, 0, 0
	}
	if used, usable, ok := p.store.PackUsage(); ok {
		p.pack.used.Store(used)
		p.pack.usable.Store(usable)
	}
	return p.pack.used.Load(), p.pack.usable.Load(), p.pack.recordBytes.Load()
}
