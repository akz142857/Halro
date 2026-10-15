package bolt

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func (s *Store) RuntimeSettings() (domain.RuntimeSettings, error) {
	var settings domain.RuntimeSettings
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyRuntimeSettings)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("decode runtime settings: %w", err)
		}
		return settings.Validate()
	})
	return settings, err
}

func (s *Store) PutRuntimeSettings(settings domain.RuntimeSettings, expectedRevision uint64) (domain.RuntimeSettings, error) {
	if err := settings.Validate(); err != nil {
		return domain.RuntimeSettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		currentRevision := uint64(0)
		if raw := bucket.Get(keyRuntimeSettings); raw != nil {
			var current domain.RuntimeSettings
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode runtime settings: %w", err)
			}
			currentRevision = current.Revision
		}
		if currentRevision != expectedRevision {
			return ErrRevisionConflict
		}
		settings.Revision = currentRevision + 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyRuntimeSettings, encoded)
	})
	return settings, err
}

func (s *Store) InstanceUISettings() (domain.InstanceUISettings, error) {
	var settings domain.InstanceUISettings
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyInstanceUISettings)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("decode instance UI settings: %w", err)
		}
		return settings.Validate()
	})
	return settings, err
}

func (s *Store) PutInstanceUISettings(settings domain.InstanceUISettings, expectedRevision uint64) (domain.InstanceUISettings, error) {
	if err := settings.Validate(); err != nil {
		return domain.InstanceUISettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		currentRevision := uint64(0)
		if raw := bucket.Get(keyInstanceUISettings); raw != nil {
			var current domain.InstanceUISettings
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode instance UI settings: %w", err)
			}
			currentRevision = current.Revision
		}
		if currentRevision != expectedRevision {
			return ErrRevisionConflict
		}
		settings.Revision = currentRevision + 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyInstanceUISettings, encoded)
	})
	return settings, err
}

func (s *Store) InstanceUsageSettings() (domain.InstanceUsageSettings, error) {
	var settings domain.InstanceUsageSettings
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyInstanceUsageSettings)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("decode instance usage settings: %w", err)
		}
		return settings.Validate()
	})
	return settings, err
}

func (s *Store) PutInstanceUsageSettings(settings domain.InstanceUsageSettings, expectedRevision uint64) (domain.InstanceUsageSettings, error) {
	if err := settings.Validate(); err != nil {
		return domain.InstanceUsageSettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		currentRevision := uint64(0)
		if raw := bucket.Get(keyInstanceUsageSettings); raw != nil {
			var current domain.InstanceUsageSettings
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode instance usage settings: %w", err)
			}
			currentRevision = current.Revision
		}
		if currentRevision != expectedRevision {
			return ErrRevisionConflict
		}
		settings.Revision = currentRevision + 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyInstanceUsageSettings, encoded)
	})
	return settings, err
}

// SeedInstanceUsageSettings writes the configured console window the first time
// an instance starts and never again.
//
// Same discipline as the accounting timezone: config.yaml is a starting point,
// not a standing instruction. Once an operator has shortened the window through
// the console — a change that destroys history and leaves an audit record — a
// later edit of the file must not silently move it back.
func (s *Store) SeedInstanceUsageSettings(consoleWindowDays int, now time.Time) (domain.InstanceUsageSettings, error) {
	settings := domain.InstanceUsageSettings{ConsoleWindowDays: consoleWindowDays, UpdatedAt: now.UTC()}
	if err := settings.Validate(); err != nil {
		return domain.InstanceUsageSettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		if raw := bucket.Get(keyInstanceUsageSettings); raw != nil {
			var current domain.InstanceUsageSettings
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode instance usage settings: %w", err)
			}
			if err := current.Validate(); err != nil {
				return err
			}
			settings = current
			return nil
		}
		settings.Revision = 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyInstanceUsageSettings, encoded)
	})
	return settings, err
}

func (s *Store) InstanceAccountingSettings() (domain.InstanceAccountingSettings, error) {
	var settings domain.InstanceAccountingSettings
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyInstanceAccountingSettings)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("decode instance accounting settings: %w", err)
		}
		return settings.Validate()
	})
	return settings, err
}

func (s *Store) PutInstanceAccountingSettings(settings domain.InstanceAccountingSettings, expectedRevision uint64) (domain.InstanceAccountingSettings, error) {
	if err := settings.Validate(); err != nil {
		return domain.InstanceAccountingSettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		currentRevision := uint64(0)
		if raw := bucket.Get(keyInstanceAccountingSettings); raw != nil {
			var current domain.InstanceAccountingSettings
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode instance accounting settings: %w", err)
			}
			currentRevision = current.Revision
			// The version is the ledger's, not the caller's: it advances only
			// when a change is applied, and a client must never be able to
			// rewind it and merge two periods that were kept apart.
			if settings.TimezoneVersion < current.TimezoneVersion {
				return fmt.Errorf("accounting timezone version cannot move backwards from %d to %d",
					current.TimezoneVersion, settings.TimezoneVersion)
			}
		}
		if currentRevision != expectedRevision {
			return ErrRevisionConflict
		}
		settings.Revision = currentRevision + 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyInstanceAccountingSettings, encoded)
	})
	return settings, err
}

// SeedInstanceAccountingSettings writes the configured zone the first time an
// instance starts and does nothing afterwards.
//
// This is the whole of config.yaml's remaining authority over the accounting
// timezone (PRD §6.2). Keeping the seed lets an unattended first deployment be
// configured from a file; refusing to reapply it keeps a later edit of that
// file from silently moving a boundary the administrator changed deliberately.
func (s *Store) SeedInstanceAccountingSettings(timezone string, now time.Time) (domain.InstanceAccountingSettings, error) {
	settings := domain.InstanceAccountingSettings{Timezone: timezone, TimezoneVersion: 1, UpdatedAt: now.UTC()}
	if err := settings.Validate(); err != nil {
		return domain.InstanceAccountingSettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		if raw := bucket.Get(keyInstanceAccountingSettings); raw != nil {
			if err := json.Unmarshal(raw, &settings); err != nil {
				return fmt.Errorf("decode instance accounting settings: %w", err)
			}
			return settings.Validate()
		}
		settings.Revision = 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyInstanceAccountingSettings, encoded)
	})
	return settings, err
}

// SeedInstanceID generates and persists a UUID-shaped identity the first
// time an instance starts, and returns the same value on every call after
// that. It exists for ADR 0015: an anchor names the instance that emitted
// it, and generating one operator-side (the way deadman's ProbeID works)
// would need cross-instance coordination this product has no other reason to
// require. candidate is only used the first time — like
// SeedInstanceAccountingSettings, a later call never overwrites what is
// already there, generated or not.
func (s *Store) SeedInstanceID(candidate string) (string, error) {
	if candidate == "" {
		return "", errors.New("candidate instance ID cannot be empty")
	}
	instanceID := candidate
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		if raw := bucket.Get(keyInstanceID); raw != nil {
			instanceID = string(raw)
			return nil
		}
		return bucket.Put(keyInstanceID, []byte(candidate))
	})
	return instanceID, err
}
