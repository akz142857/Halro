package bolt

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

// FacadeEntry describes one immediate bucket entry. Nested buckets have a nil
// value. A backend must authenticate both positive and negative reads.
type FacadeEntry struct {
	Key, Value []byte
	Bucket     bool
}

// FacadeTransaction is callback-scoped and sticky-error checked. All writes,
// cursors, range work and returned bytes must be bounded by the actual backend.
// Get's errorless Bucket wrapper checks Error before publication, so an absent
// value can never turn an ignored integrity failure into a successful write.
type FacadeTransaction interface {
	Get([]string, []byte) ([]byte, bool, error)
	BucketExists([]string) (bool, error)
	Put([]string, []byte, []byte) error
	Delete([]string, []byte) error
	CreateBucket([]string) error
	DeleteBucket([]string) error
	Seek([]string, []byte, bool, bool) (FacadeEntry, bool, error)
	Revision() (uint64, error)
	Error() error
}

// FacadeBackend owns actual native-plus-projection preparation and publication.
// Its constructor must qualify a selected root and actual Standalone source;
// this interface does not grant a role or authorize a Runtime backend switch.
type FacadeBackend interface {
	View(context.Context, func(FacadeTransaction) error) error
	Update(context.Context, func(FacadeTransaction) error) (metadatajournal.Record, error)
	Close() error
	Path() string
	JournalPath() string
	JournalState(context.Context) (JournalState, error)
	HealthError() error
}

var ErrFacadeMaintenance = errors.New("authenticated metadata requires selected-root maintenance, not raw bbolt file maintenance")

// NewFacadeStore reuses the business DAO's normalization, revision and index
// logic over a qualified authenticated backend. It owns that facade until Close.
// It performs no schema migration, source discovery, selection or role change.
// Physical snapshots, Replica apply and whole-file operations use their separate
// qualified maintenance paths and are refused by this business facade.
func NewFacadeStore(ctx context.Context, backend FacadeBackend) (*Store, error) {
	if backend == nil || ctx == nil {
		return nil, ErrFacadeMaintenance
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := backend.HealthError(); err != nil {
		return nil, err
	}
	s := &Store{facade: backend, path: backend.Path()}
	s.batches.delay, s.batches.size = metadataBatchDelay, metadataBatchSize
	var version uint64
	err := backend.View(ctx, func(tx FacadeTransaction) error {
		v, found, err := tx.Get([]string{"meta"}, keySchemaVersion)
		if err != nil {
			return err
		}
		var ok bool
		version, ok = decodeUint64(v)
		if !found || !ok || version != schemaVersion {
			return ErrSchemaVersionMismatch
		}
		revision, err := tx.Revision()
		if err != nil {
			return err
		}
		if revision == 0 || revision > uint64(math.MaxInt) {
			return ErrFacadeMaintenance
		}
		for _, name := range requiredBuckets() {
			found, err := tx.BucketExists([]string{string(name)})
			if err != nil {
				return err
			}
			if !found {
				return ErrSchemaVersionMismatch
			}
		}
		return tx.Error()
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) runFacadeUpdate(ctx context.Context, callbacks []func(*Tx) error) error {
	if err := s.RecoveryError(); err != nil {
		return err
	}
	ctx, cancel := s.confirmationContext(ctx)
	defer cancel()
	_, err := s.facade.Update(ctx, func(backend FacadeTransaction) error {
		revision, err := backend.Revision()
		if err != nil {
			return err
		}
		if revision == 0 || revision >= uint64(math.MaxInt) {
			return ErrFacadeMaintenance
		}
		r := newRecorder()
		tx := &Tx{facade: backend, recorder: r}
		for _, fn := range callbacks {
			if err := fn(tx); err != nil {
				return err
			}
		}
		if err := tx.bumpPriceRevisions(); err != nil {
			return err
		}
		if err := tx.bumpProposalRevisions(); err != nil {
			return err
		}
		if err := tx.updatePricingPendingCount(); err != nil {
			return err
		}
		return errors.Join(r.finish(), backend.Error())
	})
	return s.facadeFailure(err)
}

// A busy actual owner is unavailable, never healthy; contention alone does not
// establish native/projection uncertainty or permanently retire the DAO.
func (s *Store) facadeFailure(cause error) error {
	if cause == nil {
		return nil
	}
	health := s.facade.HealthError()
	if health != nil && !errors.Is(health, authenticatedstate.ErrBusy) {
		// A business refusal can race native withdrawal after its callback.
		// Preserve that diagnostic as text, not an ErrNotFound/Conflict branch
		// which could hide the actual integrity/authority failure in HTTP.
		return s.requireMetadataRecovery(fmt.Errorf("metadata owner became unavailable after transaction (%v): %w", cause, health))
	}
	return cause
}

// AuthenticatedMetadata reports the actual DAO backend, not a configured mode.
func (s *Store) AuthenticatedMetadata() bool { return s != nil && s.facade != nil }
