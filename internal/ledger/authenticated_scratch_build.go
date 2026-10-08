package ledger

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
)

type fixedLedgerScratchDebt struct {
	cause   error
	scratch *scratchstate.Store
	packed  *AuthenticatedLedgerProjection
	work    resourcebudget.Permit
}

func (e *fixedLedgerScratchDebt) Error() string { return e.cause.Error() }
func (e *fixedLedgerScratchDebt) Unwrap() error { return e.cause }

type scratchProjectionRows struct {
	tx     *scratchstate.Transaction
	buffer *scratchstate.RowBuffer
}

func updateBufferedLedgerRows(ctx context.Context, s *scratchstate.Store, b *scratchstate.RowBuffer, fn func(scratchProjectionRows) error) error {
	return s.Update(ctx, func(tx *scratchstate.Transaction) error {
		defer b.Reset()
		if err := fn(scratchProjectionRows{tx, b}); err != nil {
			return err
		}
		return b.Flush(tx)
	})
}

func (r scratchProjectionRows) get(bucket string, key []byte) ([]byte, bool, error) {
	k, e := authenticatedLedgerKey(bucket, key)
	if e != nil {
		return nil, false, e
	}
	if r.buffer != nil {
		return r.buffer.Get(r.tx, k)
	}
	return r.tx.Get(k)
}
func (r scratchProjectionRows) put(bucket string, key, value []byte) error {
	k, e := authenticatedLedgerKey(bucket, key)
	if e != nil {
		return e
	}
	if r.buffer != nil {
		return r.buffer.Put(r.tx, k, value)
	}
	return r.tx.Put(k, value)
}
func (r scratchProjectionRows) delete(bucket string, key []byte) error {
	k, e := authenticatedLedgerKey(bucket, key)
	if e != nil {
		return e
	}
	if r.buffer != nil {
		return r.buffer.Delete(r.tx, k)
	}
	return r.tx.Delete(k)
}
func (r scratchProjectionRows) floor(bucket string, key []byte) ([]byte, []byte, bool, error) {
	k, e := authenticatedLedgerKey(bucket, key)
	if e != nil {
		return nil, nil, false, e
	}
	if r.buffer != nil {
		if e := r.buffer.Flush(r.tx); e != nil {
			return nil, nil, false, e
		}
	}
	row, found, e := r.tx.Floor(k)
	if e != nil || !found {
		return nil, nil, false, e
	}
	if len(row.Key) < 2 || row.Key[0] != k[0] {
		return nil, nil, false, nil
	}
	return row.Key[1:], row.Value, true, nil
}

// This fresh reducer shares every domain mutation with ordinary Ledger. Only
// its private disk engine changes. The final authenticated root still contains
// the complete canonical ordinal namespace and is signed once after full native
// authentication; no scratch image is accepted as an input authority.
func buildFixedPackedAuthenticatedLedgerProjection(ctx context.Context, source string, nativeKey []byte, output string, expected ChainReport, o AuthenticatedLedgerOptions) (result *AuthenticatedLedgerProjection, resultErr error) {
	if len(nativeKey) != 32 || o.Identity == ([32]byte{}) || o.Budget == nil || expected.ChecksumOnly != 0 || o.MaxDiskBytes <= 0 {
		return nil, ErrProjectionSource
	}
	if o.Prefix != nil && (o.Prefix.Head.Offset < 0 || o.Prefix.Head.Generation == 0 && *o.Prefix != (ProjectionPrefix{})) {
		return nil, ErrProjectionCorrupt
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes + scratchstate.RowBufferRAMBytes})
	if err != nil {
		return nil, err
	}
	buffer := new(scratchstate.RowBuffer)
	var scratch *scratchstate.Store
	var packed *AuthenticatedLedgerProjection
	defer func() {
		if cleanup := scratch.Discard(); cleanup != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, cleanup)
		}
		if resultErr != nil && packed != nil {
			if cleanup := packed.Discard(); cleanup != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, cleanup)
			}
			result = nil
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			resultErr = &fixedLedgerScratchDebt{resultErr, scratch, packed, work}
			return
		}
		resultErr = finishAuthenticatedSourceCredit(work, resultErr)
		if resultErr != nil {
			result = nil
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				resultErr = &fixedLedgerScratchDebt{resultErr, scratch, packed, work}
			}
		}
	}()
	scratch, err = scratchstate.Create(ctx, output+".reducer.db", scratchstate.Options{Budget: o.Budget, MaxDiskBytes: o.MaxDiskBytes})
	if err != nil {
		return nil, err
	}
	err = updateBufferedLedgerRows(ctx, scratch, buffer, func(rows scratchProjectionRows) error {
		for _, item := range []struct {
			name  string
			value any
		}{{"export_run_count", uint64(0)}, {"export_work_unit_count", uint64(0)}, {"pending_count", int64(0)}, {"position", projectionPosition{}}} {
			if err := projectionRowsPut(rows, "meta", []byte(item.name), item.value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	seenPrefix := o.Prefix != nil && *o.Prefix == (ProjectionPrefix{})
	var prefixMismatch error
	check := func(head Watermark, hash [32]byte) error {
		if o.Prefix != nil && head == o.Prefix.Head {
			seenPrefix = true
			if hash != o.Prefix.Hash {
				prefixMismatch = ErrProjectionCorrupt
			}
		}
		return nil
	}
	var batch []Record
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := updateBufferedLedgerRows(ctx, scratch, buffer, func(rows scratchProjectionRows) error {
			for _, r := range batch {
				if err := applyProjectionRecord(ctx, rows, r); err != nil {
					return err
				}
			}
			return nil
		})
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		return err
	}
	report, partial, err := inspectReplayAuthenticated(ctx, source, nativeKey, func(r Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := check(Watermark{r.Generation, r.Offset, r.Sequence}, r.Hash); err != nil {
			return err
		}
		if o.Prefix != nil && (r.Generation > o.Prefix.Head.Generation || r.Generation == o.Prefix.Head.Generation && r.Offset > o.Prefix.Head.Offset) {
			return nil
		}
		raw, err := json.Marshal(r.Event)
		if err != nil {
			return err
		}
		if len(raw) > maxPayloadSize {
			return ErrProjectionPageLimit
		}
		if len(batch) == projectionBuildBatchRows || batchBytes+len(raw) > projectionBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, r)
		batchBytes += len(raw)
		return nil
	}, check)
	if err != nil || partial || report != expected {
		return nil, errors.Join(ErrProjectionSource, err)
	}
	if prefixMismatch != nil || o.Prefix != nil && !seenPrefix {
		return nil, errors.Join(ErrProjectionCorrupt, prefixMismatch)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	position := projectionPosition{}
	err = updateBufferedLedgerRows(ctx, scratch, buffer, func(rows scratchProjectionRows) error {
		p, found, err := projectionRowsGet[projectionPosition](rows, "meta", []byte("position"))
		if err != nil || !found {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		position = p
		head, hash, epoch := report.Head, report.ChainHash, position.Epoch
		if o.Prefix != nil {
			head = o.Prefix.Head
			hash = o.Prefix.Hash
			epoch = position.Epoch
		}
		position = projectionPosition{head, hash, epoch}
		return projectionRowsPut(rows, "meta", []byte("position"), position)
	})
	if err != nil {
		return nil, err
	}
	anchor := ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch}
	scope := authenticatedLedgerScope(o.Identity)
	key, err := hkdf.Key(sha256.New, nativeKey, scope[:], "halro:ledger:authenticated-rows:hmac:v1", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	var store *authenticatedstate.Store
	err = scratch.View(ctx, func(tx *scratchstate.Transaction) error {
		it, err := tx.Iterator()
		if err != nil {
			return err
		}
		store, err = authenticatedstate.CreateStore(ctx, output, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: authenticatedLedgerCut(scope, anchor), Key: key, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
			row, found, err := it.Next()
			return authenticatedstate.Row{Key: row.Key, Value: row.Value}, found, err
		})
		return err
	})
	if store != nil {
		path, pathErr := filepath.Abs(source)
		packed = &AuthenticatedLedgerProjection{store: store, scope: scope, budget: o.Budget, nativePath: path, nativeKeyTag: sha256.Sum256(nativeKey)}
		packed.closeContext, packed.closeCancel = context.WithCancel(context.Background())
		err = errors.Join(err, pathErr)
	}
	if err != nil {
		return nil, err
	}
	report, partial, err = InspectReplayAuthenticatedContext(ctx, source, nativeKey, nil)
	if err != nil || partial || report != expected {
		return nil, errors.Join(ErrProjectionSource, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	packed.ready = true
	return packed, nil
}
