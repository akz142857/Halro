package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func outcomeDefinitionKey(id string, version uint64) []byte {
	key := make([]byte, len(id)+1+8)
	copy(key, id)
	binary.BigEndian.PutUint64(key[len(id)+1:], version)
	return key
}

func (s *Store) PutOutcomeDefinition(ctx context.Context, definition domain.OutcomeDefinition, expectedProjectRevision uint64, expectedDefinitionRevision uint64, intent *domain.AdminAuditIntent) (domain.OutcomeDefinition, error) {
	if err := definition.Validate(); err != nil {
		return domain.OutcomeDefinition{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.OutcomeDefinition{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		var project domain.Project
		rawProject := tx.Bucket(bucketProjects).Get([]byte(definition.ProjectID))
		if rawProject == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(rawProject, &project); err != nil {
			return err
		}
		if project.DeletedAt != nil || project.Revision != expectedProjectRevision {
			return ErrRevisionConflict
		}
		bucket := tx.Bucket(bucketOutcomeDefinitions)
		var latest *domain.OutcomeDefinition
		latestByID := map[string]domain.OutcomeDefinition{}
		if err := bucket.ForEach(func(_, value []byte) error {
			if value == nil {
				return nil
			}
			var item domain.OutcomeDefinition
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			if item.ProjectID != definition.ProjectID {
				return nil
			}
			if current, ok := latestByID[item.ID]; !ok || item.Version > current.Version {
				latestByID[item.ID] = item
			}
			if item.ID == definition.ID && (latest == nil || item.Version > latest.Version) {
				copy := item
				latest = &copy
			}
			return nil
		}); err != nil {
			return err
		}
		active := 0
		for _, item := range latestByID {
			if item.Enabled {
				active++
			}
		}
		if latest == nil {
			if definition.Version != 1 || expectedDefinitionRevision != 0 {
				return ErrNotFound
			}
			if definition.Enabled && active >= domain.MaxActiveOutcomeDefinitions {
				return errors.New("active outcome definition limit exceeded")
			}
			definition.Revision = 1
		} else {
			if expectedDefinitionRevision == 0 || latest.Revision != expectedDefinitionRevision {
				return ErrRevisionConflict
			}
			if definition.Version != latest.Version+1 || definition.Name != latest.Name || definition.ProjectID != latest.ProjectID {
				return errors.New("outcome definition version identity is immutable")
			}
			if !latest.Enabled && definition.Enabled && active >= domain.MaxActiveOutcomeDefinitions {
				return errors.New("active outcome definition limit exceeded")
			}
			definition.Revision = latest.Revision + 1
		}
		encoded, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		if bucket.Get(outcomeDefinitionKey(definition.ID, definition.Version)) != nil {
			return ErrAlreadyExists
		}
		if err := bucket.Put(outcomeDefinitionKey(definition.ID, definition.Version), encoded); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return definition, err
}

func (s *Store) GetOutcomeDefinition(ctx context.Context, projectID, id string, version uint64) (domain.OutcomeDefinition, error) {
	var result domain.OutcomeDefinition
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketOutcomeDefinitions)
		if version != 0 {
			raw := bucket.Get(outcomeDefinitionKey(id, version))
			if raw == nil {
				return ErrNotFound
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			if projectID != "" && result.ProjectID != projectID {
				return ErrNotFound
			}
			return nil
		}
		return bucket.ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var item domain.OutcomeDefinition
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if item.ID == id && (projectID == "" || item.ProjectID == projectID) && item.Version > result.Version {
				result = item
			}
			return nil
		})
	})
	if err != nil {
		return domain.OutcomeDefinition{}, err
	}
	if result.ID == "" {
		return domain.OutcomeDefinition{}, ErrNotFound
	}
	return result, nil
}

func (s *Store) ListOutcomeDefinitions(ctx context.Context, projectID string) ([]domain.OutcomeDefinition, error) {
	items := []domain.OutcomeDefinition{}
	err := s.listJSON(ctx, bucketOutcomeDefinitions, func(raw []byte) error {
		var item domain.OutcomeDefinition
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("decode outcome definition: %w", err)
		}
		if projectID == "" || item.ProjectID == projectID {
			items = append(items, item)
		}
		return nil
	})
	sort.Slice(items, func(i, j int) bool {
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		return items[i].Version < items[j].Version
	})
	return items, err
}

type GovernanceCheckpoint struct {
	Version  int      `json:"version"`
	Sequence uint64   `json:"sequence"`
	Offset   int64    `json:"offset"`
	Segments []string `json:"segments"`
	Payload  []byte   `json:"-"`
}

const governanceCheckpointSegmentSize = 4 << 20

func (s *Store) SaveGovernanceCheckpoint(sequence uint64, offset int64, payload []byte) error {
	value := GovernanceCheckpoint{Version: 1, Sequence: sequence, Offset: offset, Payload: payload}
	if value.Version <= 0 || value.Sequence == 0 || value.Offset <= 0 || len(value.Payload) == 0 {
		return errors.New("governance checkpoint is invalid")
	}
	for start := 0; start < len(payload); start += governanceCheckpointSegmentSize {
		end := start + governanceCheckpointSegmentSize
		if end > len(payload) {
			end = len(payload)
		}
		digest := sha256.Sum256(payload[start:end])
		value.Segments = append(value.Segments, hex.EncodeToString(digest[:]))
	}
	value.Payload = nil
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		segments := tx.Bucket(bucketGovernanceCheckpointSegments)
		for index, digest := range value.Segments {
			start, end := index*governanceCheckpointSegmentSize, (index+1)*governanceCheckpointSegmentSize
			if end > len(payload) {
				end = len(payload)
			}
			if current := segments.Get([]byte(digest)); current != nil && !bytes.Equal(current, payload[start:end]) {
				return errors.New("governance checkpoint hash collision")
			} else if current != nil {
				continue
			}
			if err := segments.Put([]byte(digest), payload[start:end]); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketMeta).Put(keyGovernanceCheckpoint, encoded)
	})
}

func (s *Store) LoadGovernanceCheckpoint() (GovernanceCheckpoint, error) {
	var value GovernanceCheckpoint
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyGovernanceCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Version != 1 || value.Sequence == 0 || value.Offset <= 0 || len(value.Segments) == 0 {
			return errors.New("governance checkpoint head is invalid")
		}
		segments := tx.Bucket(bucketGovernanceCheckpointSegments)
		for _, expected := range value.Segments {
			segment := segments.Get([]byte(expected))
			if segment == nil {
				return errors.New("governance checkpoint segment is missing")
			}
			digest := sha256.Sum256(segment)
			if hex.EncodeToString(digest[:]) != expected {
				return errors.New("governance checkpoint segment checksum mismatch")
			}
			value.Payload = append(value.Payload, segment...)
		}
		return nil
	})
	return value, err
}

func (s *Store) ResetGovernanceCheckpoint() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bucketMeta).Delete(keyGovernanceCheckpoint); err != nil {
			return err
		}
		if err := tx.DeleteBucket(bucketGovernanceCheckpointSegments); err != nil && !errors.Is(err, bbolt.ErrBucketNotFound) {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(bucketGovernanceCheckpointSegments)
		return err
	})
}
