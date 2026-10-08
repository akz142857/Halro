package ledger

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"math"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

type packedLedgerBuildDebt struct {
	cause      error
	scratch    *LedgerProjection
	packed     *AuthenticatedLedgerProjection
	work, disk resourcebudget.Permit
}

type freshProjectionRetirement struct {
	cause error
	owner *LedgerProjection
}

func (e *freshProjectionRetirement) Error() string { return e.cause.Error() }
func (e *freshProjectionRetirement) Unwrap() error { return e.cause }
func retainFreshProjectionCleanupDebt(owner *LedgerProjection, cause, cleanup error) error {
	if cleanup != nil {
		return &freshProjectionRetirement{errors.Join(cause, cleanup, ownedfile.ErrRetirement), owner}
	}
	return cause
}

func (e *packedLedgerBuildDebt) Error() string { return e.cause.Error() }
func (e *packedLedgerBuildDebt) Unwrap() error { return e.cause }

// BuildPackedAuthenticatedLedgerProjection reduces the independently frozen
// native source into a NEW private bounded scratch file, then streams its exact
// ordinal namespace into one authenticated pack. No saved DB is accepted as a
// baseline. Unlike per-fact COW construction, initial disk use retains only the
// final rows. Live mutation and selected-root reopening use the same codec.
func BuildPackedAuthenticatedLedgerProjection(ctx context.Context, source string, nativeKey []byte, output string, expected ChainReport, o AuthenticatedLedgerOptions) (result *AuthenticatedLedgerProjection, resultErr error) {
	return buildFixedPackedAuthenticatedLedgerProjection(ctx, source, nativeKey, output, expected, o)
}

func buildPackedAuthenticatedLedgerProjection(ctx context.Context, source string, nativeKey []byte, output string, expected ChainReport, o AuthenticatedLedgerOptions, phase func(*LedgerProjection) error) (result *AuthenticatedLedgerProjection, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(nativeKey) != 32 || o.Identity == ([32]byte{}) || o.Budget == nil || expected.ChecksumOnly != 0 || o.MaxDiskBytes <= 0 || o.MaxDiskBytes > math.MaxInt64-(128<<20) {
		return nil, ErrProjectionSource
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: o.MaxDiskBytes + (128 << 20)})
	if err != nil {
		return nil, err
	}
	disk, err := o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		return nil, errors.Join(err, work.Release())
	}
	var scratch *LedgerProjection
	var packed *AuthenticatedLedgerProjection
	defer func() {
		cleanupErr := scratch.Discard()
		resultErr = errors.Join(resultErr, cleanupErr)
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement)
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			result = nil
			resultErr = &packedLedgerBuildDebt{resultErr, scratch, packed, work, disk}
			return
		}
		if resultErr != nil && packed != nil {
			if cleanupErr := packed.Discard(); cleanupErr != nil {
				resultErr = errors.Join(resultErr, cleanupErr, ownedfile.ErrRetirement)
			}
			result = nil
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &packedLedgerBuildDebt{resultErr, scratch, packed, work, disk}
			return
		}
		resultErr = errors.Join(resultErr, disk.Release(), work.Release())
		if resultErr != nil {
			result = nil
			// A failed fee transfer must not lose the still-live packed owner.
			// Keep the complete retirement handle even if another permit already
			// reached its release barrier.
			resultErr = &packedLedgerBuildDebt{resultErr, scratch, packed, work, disk}
		}
	}()
	scratchPath := output + ".reducer.db"
	if o.Prefix == nil {
		scratch, err = BuildLedgerProjectionWithLimit(ctx, source, nativeKey, scratchPath, expected, o.MaxDiskBytes)
	} else {
		scratch, err = BuildLedgerProjectionPrefixWithLimit(ctx, source, nativeKey, scratchPath, expected, *o.Prefix, o.MaxDiskBytes)
	}
	if err != nil {
		var debt *freshProjectionRetirement
		if errors.As(err, &debt) {
			scratch = debt.owner
		}
		return nil, err
	}
	if err := scratch.file.VerifyName(); err != nil {
		return nil, err
	}
	if phase != nil {
		if err := phase(scratch); err != nil {
			return nil, err
		}
	}
	anchor, err := scratch.ReadReplayAnchor(ctx)
	if err != nil {
		return nil, err
	}
	scope := authenticatedLedgerScope(o.Identity)
	key, err := hkdf.Key(sha256.New, nativeKey, scope[:], "halro:ledger:authenticated-rows:hmac:v1", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	var store *authenticatedstate.Store
	err = scratch.db.View(func(tx *bbolt.Tx) error {
		bucketIndex := 0
		var cursor *bbolt.Cursor
		var rawKey, rawValue []byte
		buckets := append(append([]string{}, projectionBuckets...), projectionListBuckets...)
		var buildErr error
		store, buildErr = authenticatedstate.CreateStore(ctx, output, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: authenticatedLedgerCut(scope, anchor), Key: key, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
			for bucketIndex < len(buckets) {
				if err := ctx.Err(); err != nil {
					return authenticatedstate.Row{}, false, err
				}
				if cursor == nil {
					bucket := tx.Bucket([]byte(buckets[bucketIndex]))
					if bucket == nil {
						return authenticatedstate.Row{}, false, ErrProjectionCorrupt
					}
					cursor = bucket.Cursor()
					rawKey, rawValue = cursor.First()
				}
				if rawKey == nil {
					bucketIndex++
					cursor = nil
					continue
				}
				if rawValue == nil || len(rawValue) > projectionMaxValue {
					return authenticatedstate.Row{}, false, ErrProjectionCorrupt
				}
				k, err := authenticatedLedgerKey(buckets[bucketIndex], rawKey)
				row := authenticatedstate.Row{Key: k, Value: rawValue}
				rawKey, rawValue = cursor.Next()
				return row, err == nil, err
			}
			return authenticatedstate.Row{}, false, nil
		})
		return buildErr
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
	if err := scratch.file.VerifyName(); err != nil {
		return nil, err
	}
	// Reauthenticate the original bytes after the complete import. The caller's
	// maintenance lease remains responsible for native mutation exclusion.
	report, partial, err := InspectReplayAuthenticatedContext(ctx, source, nativeKey, nil)
	if err != nil || partial || report != expected {
		return nil, errors.Join(ErrProjectionSource, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	packed.ready = true
	return packed, nil
}
