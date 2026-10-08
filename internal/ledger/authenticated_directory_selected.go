package ledger

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// OpenSelectedAuthenticatedDirectory requires the actual native selected source
// loan. OpenWithSelectedSeal already reconciles all physical descriptors against
// this selected authenticated namespace. Completed rolled suffixes add descriptors in fixed pages under the same
// native freeze; trim and alternate roots cannot borrow that qualification.
func OpenSelectedAuthenticatedDirectory(ctx context.Context, native *Log, selected AuthenticatedDirectoryReference, output string, o AuthenticatedDirectoryOptions, sourceOptions SelectedLedgerProjectionOpenOptions) (result *AuthenticatedDirectory, resultErr error) {
	if native == nil || native.startupProvenance == nil || native.startupProvenance.ha != nil {
		return nil, ErrProjectionSource
	}
	return openSelectedAuthenticatedDirectory(ctx, native, selected, output, o, sourceOptions, false)
}

// OpenHASelectedAuthenticatedDirectory joins only the actual native
// constructor's independent Member/Ordering/source authority. Saved role or
// prefix fields cannot confer this permission.
func OpenHASelectedAuthenticatedDirectory(ctx context.Context, native *Log, selected AuthenticatedDirectoryReference, output string, o AuthenticatedDirectoryOptions, sourceOptions SelectedLedgerProjectionOpenOptions) (result *AuthenticatedDirectory, resultErr error) {
	if native == nil || native.startupProvenance == nil || native.startupProvenance.ha == nil {
		return nil, ErrProjectionSource
	}
	return openSelectedAuthenticatedDirectory(ctx, native, selected, output, o, sourceOptions, true)
}

func openSelectedAuthenticatedDirectory(ctx context.Context, native *Log, selected AuthenticatedDirectoryReference, output string, o AuthenticatedDirectoryOptions, sourceOptions SelectedLedgerProjectionOpenOptions, ha bool) (result *AuthenticatedDirectory, resultErr error) {
	if native == nil || o.Budget == nil || o.Identity == ([32]byte{}) || selected.Schema != authenticatedDirectorySchema {
		return nil, ErrProjectionSource
	}
	anchor, err := directoryAnchorFromPosition(selected.Position)
	if err != nil {
		return nil, err
	}
	withOwner := native.WithSelectedStartupOwner
	if ha {
		withOwner = native.WithHASelectedStartupOwner
		if sourceOptions.PhysicalRecovery != nil {
			withOwner = func(ctx context.Context, budget *resourcebudget.Manager, options SelectedLedgerProjectionOpenOptions, identity [32]byte, visit func(context.Context, SelectedStartupSource) error) error {
				return native.WithHASelectedPhysicalStartupOwner(ctx, budget, options, identity, func(ctx context.Context, source SelectedHAPhysicalStartupSource) error { return visit(ctx, source) })
			}
		}
	}
	resultErr = withOwner(ctx, o.Budget, sourceOptions, o.Identity, func(ctx context.Context, source SelectedStartupSource) error {
		seal := source.SelectedSeal()
		var root authenticatedstate.SealRoot
		for _, r := range seal.Roots[:seal.RootCount] {
			if r.Component == authenticatedDirectorySchema.Component && r.IndexID == authenticatedDirectorySchema.IndexID && r.Codec == authenticatedDirectorySchema.Codec {
				root = r
			}
		}
		head := source.NativeHead()
		if root != selected.Root || source.SavedAnchor() != anchor || head.Head.Generation < anchor.Head.Generation {
			return ErrProjectionSource
		}
		if err := source.VerifyAnchors(ctx, [2]ReplayAnchor{anchor, head}); err != nil {
			return err
		}
		scope := authenticatedDirectoryScope(o.Identity)
		key, err := hkdf.Key(sha256.New, native.chainKey, scope[:], "halro:ledger:authenticated-directory:hmac:v1", 32)
		if err != nil {
			return err
		}
		defer clear(key)
		schema := authenticatedDirectorySchema
		store, err := authenticatedstate.OpenStore(ctx, output, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: authenticatedDirectoryCut(scope, anchor), Key: key, Budget: o.Budget, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, DiskReservation: o.DiskReservation, ExpectedSealRoot: &root, ExpectedSealSchema: &schema, PhysicalRecovery: sourceOptions.PhysicalRecovery != nil})
		if store != nil {
			result = newAuthenticatedDirectory(store, anchor, anchor.Head.Generation-1, o)
			result.selectedNative = native
		}
		if err != nil {
			return err
		}
		pin, err := store.Snapshot(ctx)
		if err != nil {
			return err
		}
		raw, found, readErr := pin.Get(ctx, []byte{0})
		if err := errors.Join(readErr, pin.Close()); err != nil || !found {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		var state authenticatedDirectoryState
		if err := json.Unmarshal(raw, &state); err != nil || state.Version != 1 || state.Anchor != anchor || state.Segments != anchor.Head.Generation-1 || root.Rows != state.Segments+1 {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		if sourceOptions.PhysicalRecovery != nil {
			restore := result.restorePhysicalDirectoryLocked
			if ha {
				restore = result.restoreHAPhysicalDirectoryLocked
			}
			if restored, err := restore(ctx, source); err != nil {
				return err
			} else if restored {
				anchor = result.anchor
				state.Anchor, state.Segments = anchor, result.segments
			}
		}
		if head != anchor {
			// New descriptors come only from the actual native owner whose suffix
			// was authenticated from the selected anchor. The common seal stays
			// unchanged until all fixed pages and the final scalar row exist.
			for first := anchor.Head.Generation; first < head.Head.Generation; {
				end := first + min(uint64(63), head.Head.Generation-first)
				var rows [63]authenticatedstate.Row
				n := 0
				pin, err := store.Snapshot(ctx)
				if err != nil {
					return err
				}
				for g := first; g < end; g++ {
					segment, found, readErr := native.lookupSegmentLocked(g)
					if readErr != nil || !found {
						err = errors.Join(ErrSegmentMissing, readErr)
						break
					}
					key := authenticatedDirectoryKey(g)
					raw, marshalErr := json.Marshal(segment)
					old, exists, readErr := pin.Get(ctx, key)
					if marshalErr != nil || readErr != nil {
						err = errors.Join(marshalErr, readErr)
						break
					}
					if !exists || !bytes.Equal(old, raw) {
						rows[n] = authenticatedstate.Row{Key: key, Value: raw}
						n++
					}
				}
				if err = errors.Join(err, pin.Close()); err != nil {
					return err
				}
				if n == 0 {
					first = end
					continue
				}
				err = result.physicalCatchupUpdateLocked(ctx, func(ctx context.Context) error {
					_, err := store.UpdateDerived(ctx, authenticatedDirectoryCut(scope, anchor), func(tx *authenticatedstate.Transaction) error {
						for _, row := range rows[:n] {
							if err := tx.Put(row.Key, row.Value); err != nil {
								return err
							}
						}
						return nil
					}, func() error { return source.Check(ctx) })
					return err
				})
				if err != nil {
					return err
				}
				first = end
			}
			state.Anchor, state.Segments = head, head.Head.Generation-1
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			err = result.physicalCatchupUpdateLocked(ctx, func(ctx context.Context) error {
				_, err := store.UpdateNativeFrame(ctx, authenticatedDirectoryCut(scope, head), func(tx *authenticatedstate.Transaction) error { return tx.Put([]byte{0}, raw) }, func() error { return source.Check(ctx) })
				return err
			})
			if err != nil {
				return err
			}
			result.anchor, result.segments = head, state.Segments
		}
		// OpenWithSelectedSeal qualified any raw-to-gzip change against the
		// independently signed plaintext descriptor. Refresh those physical
		// locators in finite pages; the common seal is still unchanged until the
		// complete seven-root publication. Failed pages are never a new authority.
		if err := refreshSelectedPhysicalDescriptors(ctx, store, native, source, scope, head, anchor.Head.Generation-1, result); err != nil {
			return err
		}
		// The file identity check alone cannot detect an in-place suffix rewrite
		// while the authenticated directory root was being opened or advanced.
		if err := source.VerifyAnchors(ctx, [2]ReplayAnchor{anchor, head}); err != nil {
			return err
		}
		return source.Check(ctx)
	})
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) && !errors.Is(resultErr, ErrSegmentRetirement) {
			result = nil
		}
	}
	return result, resultErr
}

func refreshSelectedPhysicalDescriptors(ctx context.Context, store *authenticatedstate.Store, native *Log, source SelectedStartupSource, scope [32]byte, head ReplayAnchor, count uint64, directory *AuthenticatedDirectory) error {
	for generation := uint64(1); generation <= count; {
		var rows [63]authenticatedstate.Row
		n := 0
		pin, err := store.Snapshot(ctx)
		if err != nil {
			return err
		}
		for generation <= count && n < len(rows) {
			segment, found, readErr := native.lookupSegmentLocked(generation)
			if readErr != nil || !found {
				err = errors.Join(ErrSegmentMissing, readErr)
				break
			}
			key := authenticatedDirectoryKey(generation)
			raw, marshalErr := json.Marshal(segment)
			old, oldFound, readErr := pin.Get(ctx, key)
			if marshalErr != nil || readErr != nil || !oldFound {
				err = errors.Join(ErrProjectionCorrupt, marshalErr, readErr)
				break
			}
			if !bytes.Equal(raw, old) {
				rows[n] = authenticatedstate.Row{Key: key, Value: raw}
				n++
			}
			generation++
		}
		if err := errors.Join(err, pin.Close()); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		err = directory.physicalCatchupUpdateLocked(ctx, func(ctx context.Context) error {
			_, err := store.UpdateDerived(ctx, authenticatedDirectoryCut(scope, head), func(tx *authenticatedstate.Transaction) error {
				for _, row := range rows[:n] {
					if err := tx.Put(row.Key, row.Value); err != nil {
						return err
					}
				}
				return nil
			}, func() error { return source.Check(ctx) })
			return err
		})
		if err != nil {
			return err
		}
	}
	return source.Check(ctx)
}
