package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

const (
	UsageSummaryLabelMaxKeys     = 101
	UsageSummaryLabelKeyBytes    = 1 << 20
	UsageSummaryLabelSourceBytes = 16 << 20
	UsageSummaryLabelOutputBytes = 1 << 20
)

var ErrUsageSummaryLabelsLimit = errors.New("usage summary labels exceed limits")

// UsageSummaryLabels reads only the displayed dimension's keys. Missing or
// folded resources have no label. Names are current Metadata observations,
// independent of the Summary accounting cut. Logical source/output limits do
// not establish physical memory fees. No provider normalization or list scan.
func (s *Store) UsageSummaryLabels(ctx context.Context, dimension string, keys []string) (map[string]string, error) {
	if len(keys) > UsageSummaryLabelMaxKeys {
		return nil, ErrUsageSummaryLabelsLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keyBytes := 0
	for _, key := range keys {
		if len(key) > UsageSummaryLabelKeyBytes-keyBytes {
			return nil, ErrUsageSummaryLabelsLimit
		}
		keyBytes += len(key)
	}
	labels := make(map[string]string)
	var bucket []byte
	switch dimension {
	case domain.RollupDimensionProject:
		bucket = bucketProjects
	case domain.RollupDimensionDeployment:
		bucket = bucketDeployments
	default:
		if dimension != "" && !domain.IsRollupDimension(dimension) {
			return nil, ErrUsageSummaryLabelsLimit
		}
		return labels, nil
	}
	seen := make(map[string]struct{}, len(keys))
	sourceBytes, outputBytes := 0, 2
	for _, key := range keys {
		if key == domain.RollupOtherKey {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		var raw []byte
		if err := s.view(func(tx *Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			value := tx.Bucket(bucket).Get([]byte(key))
			if len(value) > metadatajournal.MaxPayloadSize || len(value)+len(key) > UsageSummaryLabelSourceBytes-sourceBytes {
				return ErrUsageSummaryLabelsLimit
			}
			sourceBytes += len(value) + len(key)
			raw = bytes.Clone(value)
			return nil
		}); err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var record struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		if record.ID != key {
			return nil, errors.New("usage label identity mismatch")
		}
		if dimension == domain.RollupDimensionDeployment && strings.TrimSpace(record.Name) == "" {
			continue
		}
		pair, err := json.Marshal([]string{key, record.Name})
		if err != nil {
			return nil, err
		}
		cost := len(pair) - 2 // [key,name] -> key:name
		if len(labels) > 0 {
			cost++
		}
		if cost > UsageSummaryLabelOutputBytes-outputBytes {
			return nil, ErrUsageSummaryLabelsLimit
		}
		outputBytes += cost
		labels[key] = record.Name
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}
