package metadatastate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// TransactionRAMBytes covers a complete bounded operations overlay, copied
// callback results and JSON serialization. Native preparation has its own loan.
const TransactionRAMBytes int64 = metadatalimits.TransactionRAMBytes

var ErrReadOnly = errors.New("metadata transaction is read-only")

// Transaction is a callback-scoped Metadata facade. Reads authenticate the
// selected immutable root and see earlier writes in this transaction. All
// errors are sticky, including errors ignored by a callback. There is no raw
// database handle, implicit whole-image buffer or caller-supplied position.
// This facade does not establish Runtime role, HA confirmation or migration
// authority. A/B/D rows enter format2 reduction; local C/E rows change only the
// authenticated projection at the same native cut.
type Transaction struct {
	mu                                 sync.Mutex
	ctx                                context.Context
	pin                                *authenticatedstate.Snapshot
	writable, closed                   bool
	err                                error
	ops                                []metadatajournal.Op
	encodedBytes, returnedBytes, calls int
	revision                           uint64
	authority                          func() error
}

func (t *Transaction) fail(err error) error {
	t.err = errors.Join(t.err, err)
	return t.err
}
func (t *Transaction) enter() error {
	if t.closed {
		return authenticatedstate.ErrClosed
	}
	if t.err != nil {
		return t.err
	}
	if t.authority != nil {
		if err := t.authority(); err != nil {
			return t.fail(err)
		}
	}
	if err := t.ctx.Err(); err != nil {
		return t.fail(err)
	}
	if t.calls >= 4096 {
		return t.fail(authenticatedstate.ErrLimit)
	}
	t.calls++
	return nil
}
func facadePath(path []string) ([][]byte, error) {
	if len(path) == 0 || len(path) > maxBaselineDepth {
		return nil, authenticatedstate.ErrLimit
	}
	p := make([][]byte, len(path))
	for i, part := range path {
		if len(part) == 0 || len(part) > 32768 {
			return nil, authenticatedstate.ErrLimit
		}
		p[i] = []byte(part)
	}
	_, err := RowKey(p, nil, true)
	return p, err
}
func hasPathPrefix(path, prefix []string) bool {
	if len(prefix) > len(path) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}
func (t *Transaction) read(path []string, key []byte, marker bool) ([]byte, bool, error) {
	p, err := facadePath(path)
	if err != nil {
		return nil, false, t.fail(err)
	}
	if _, err := boltstore.MetadataBaselineClass(path, key); err != nil {
		return nil, false, t.fail(err)
	}
	raw, err := RowKey(p, key, marker)
	if err != nil {
		return nil, false, t.fail(err)
	}
	for i := len(t.ops) - 1; i >= 0; i-- {
		op := t.ops[i]
		if op.Kind == metadatajournal.OpDeleteBucket && hasPathPrefix(path, op.Path) {
			return nil, false, nil
		}
		if !hasPathPrefix(path, op.Path) || len(path) != len(op.Path) {
			continue
		}
		if marker && op.Kind == metadatajournal.OpCreateBucket {
			return make([]byte, 8), true, nil
		}
		if !marker && bytes.Equal(key, op.Key) {
			if op.Kind == metadatajournal.OpDelete {
				return nil, false, nil
			}
			if op.Kind == metadatajournal.OpPut {
				return t.returnValue(op.Value, true, nil)
			}
		}
	}
	value, found, err := t.pin.Get(t.ctx, raw)
	return t.returnValue(value, found, err)
}
func (t *Transaction) returnValue(value []byte, found bool, err error) ([]byte, bool, error) {
	if err != nil {
		return nil, false, t.fail(err)
	}
	if t.authority != nil {
		if err := t.authority(); err != nil {
			return nil, false, t.fail(err)
		}
	}
	if len(value) > metadatajournal.MaxPayloadSize-t.returnedBytes {
		return nil, false, t.fail(authenticatedstate.ErrLimit)
	}
	t.returnedBytes += len(value)
	return bytes.Clone(value), found, nil
}

func (t *Transaction) Error() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return authenticatedstate.ErrClosed
	}
	return errors.Join(t.err, t.ctx.Err())
}
func (t *Transaction) Revision() (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return 0, err
	}
	return t.revision, nil
}

// Context is cancelled when the caller or Metadata owner closes.
func (t *Transaction) Context() context.Context { return t.ctx }

func (t *Transaction) Get(path []string, key []byte) ([]byte, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return nil, false, err
	}
	return t.read(path, key, false)
}
func (t *Transaction) BucketExists(path []string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return false, err
	}
	_, found, err := t.read(path, nil, true)
	return found, err
}
func (t *Transaction) add(op metadatajournal.Op) error {
	if !t.writable {
		return t.fail(ErrReadOnly)
	}
	if len(t.ops) >= 1024 {
		return t.fail(authenticatedstate.ErrLimit)
	}
	if err := validateFacadeOperations([]metadatajournal.Op{op}); err != nil {
		return t.fail(err)
	}
	encoded, err := json.Marshal(op)
	if err != nil {
		return t.fail(err)
	}
	// Include array punctuation before copying caller-owned input. Checking the
	// complete encoding rather than raw value bytes also covers base64 expansion.
	if len(encoded)+1 > metadatajournal.MaxPayloadSize-2-t.encodedBytes {
		return t.fail(authenticatedstate.ErrLimit)
	}
	t.encodedBytes += len(encoded) + 1
	op.Path = append([]string(nil), op.Path...)
	op.Key, op.Value = bytes.Clone(op.Key), bytes.Clone(op.Value)
	t.ops = append(t.ops, op)
	return nil
}
func (t *Transaction) Put(path []string, key, value []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return err
	}
	_, found, err := t.read(path, nil, true)
	if err != nil {
		return err
	}
	if !found {
		return t.fail(ErrSource)
	}
	if len(path) < maxBaselineDepth && path[0] != "meta" {
		child := append(append([]string(nil), path...), string(key))
		if _, collision, err := t.read(child, nil, true); err != nil {
			return err
		} else if collision {
			return t.fail(ErrSource)
		}
	}
	return t.add(metadatajournal.Op{Kind: metadatajournal.OpPut, Path: path, Key: key, Value: value})
}
func (t *Transaction) Delete(path []string, key []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return err
	}
	return t.add(metadatajournal.Op{Kind: metadatajournal.OpDelete, Path: path, Key: key})
}
func (t *Transaction) CreateBucket(path []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return err
	}
	if len(path) > 1 {
		if _, found, err := t.read(path[:len(path)-1], nil, true); err != nil {
			return err
		} else if !found {
			return t.fail(ErrSource)
		}
		if _, collision, err := t.read(path[:len(path)-1], []byte(path[len(path)-1]), false); err != nil {
			return err
		} else if collision {
			return t.fail(ErrSource)
		}
	}
	if _, found, err := t.read(path, nil, true); err != nil {
		return err
	} else if found {
		return nil
	}
	return t.add(metadatajournal.Op{Kind: metadatajournal.OpCreateBucket, Path: path})
}
func (t *Transaction) DeleteBucket(path []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return err
	}
	return t.add(metadatajournal.Op{Kind: metadatajournal.OpDeleteBucket, Path: path})
}

// View joins the complete callback on Close and revokes escaped transactions.
func (b *Baseline) View(ctx context.Context, visit func(*Transaction) error) error {
	_, err := b.transact(ctx, false, visit)
	return err
}

// Update stages at most one format2 frame. Callback/limit refusals leave the
// existing root and journal healthy and unchanged. Native/root uncertainty
// follows AppendFrame's independently qualified recovery protocol.
func (b *Baseline) Update(ctx context.Context, visit func(*Transaction) error) (metadatajournal.Record, error) {
	return b.transact(ctx, true, visit)
}
func (b *Baseline) transact(ctx context.Context, writable bool, visit func(*Transaction) error) (result metadatajournal.Record, resultErr error) {
	if b == nil || visit == nil || b.haPeerTransition != nil || writable && b.readOnly {
		return result, ErrSource
	}
	if !b.mu.TryLock() {
		return result, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	defer func() {
		if errors.Is(resultErr, authenticatedstate.ErrCorrupt) || errors.Is(resultErr, authenticatedstate.ErrRecovery) {
			b.ready.Store(false)
		}
	}()
	if !b.ready.Load() || !b.linked || b.journal == nil {
		return result, authenticatedstate.ErrClosed
	}
	if check, ok := ctx.Value(haFacadeReadCheckKey{}).(haFacadeReadCheck); ok {
		if writable || !check.matches(metadataSealPosition(b.position)) {
			return result, ErrSource
		}
	}
	if b.selectedStartup {
		if err := b.journal.HealthError(); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	fee, err := b.physicalWork(ctx, resourcebudget.Cost{RAM: TransactionRAMBytes})
	if err != nil {
		return result, err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			b.retirementErr = errors.Join(b.retirementErr, resultErr)
			b.transactionDebt = fee
			b.ready.Store(false)
			return
		}
		releaseErr := fee.Release()
		resultErr = errors.Join(resultErr, releaseErr)
		if releaseErr != nil {
			b.transactionDebt = fee
			b.retirementErr = errors.Join(b.retirementErr, releaseErr)
			b.ready.Store(false)
		}
		if resultErr != nil {
			result = metadatajournal.Record{}
		}
	}()
	pin, err := b.store.Snapshot(ctx)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		return result, err
	}
	tx := &Transaction{ctx: ctx, pin: pin, writable: writable, revision: root.Revision}
	if b.selectedStartup {
		tx.authority = b.journal.HealthError
	}
	defer func() { tx.mu.Lock(); tx.closed = true; tx.ops = nil; tx.pin = nil; tx.mu.Unlock() }()
	err = visit(tx)
	tx.mu.Lock()
	tx.closed = true
	if tx.err != nil {
		err = tx.err
	} else if ctx.Err() != nil {
		err = ctx.Err()
	}
	ops := tx.ops
	tx.mu.Unlock()
	if b.selectedStartup {
		if authorityErr := b.journal.HealthError(); authorityErr != nil {
			return result, authorityErr
		}
	}
	if err != nil {
		return result, err
	}
	if !writable || len(ops) == 0 {
		return result, nil
	}
	// Release the read pin before mutable preparation. It pins the same root
	// selected under b.mu, so no other owner operation can cross the callback.
	if err := pin.Close(); err != nil {
		return result, err
	}
	return b.commitFacadeLocked(ctx, ops)
}
