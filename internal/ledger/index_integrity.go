package ledger

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
)

// Projection balances always persist all five scalar fields, including zeros.
// Missing or null fields are corruption, not an empty account. Keeping this
// decoder local to projections does not change native facts or snapshot formats.
func decodeProjectionValue[T any](raw []byte) (value T, err error) {
	if len(raw) > projectionMaxValue || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return value, ErrProjectionCorrupt
	}
	if digest, ok := any(&value).(*[32]byte); ok {
		// Unmarshalling directly into a fixed array silently pads short input
		// and truncates long input. Neither is a persisted SHA-256 index row.
		var encoded []byte
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '[' || json.Unmarshal(raw, &encoded) != nil || len(encoded) != len(digest) {
			return value, ErrProjectionCorrupt
		}
		copy(digest[:], encoded)
		return value, nil
	}
	if balance, ok := any(&value).(*Balance); ok {
		var fields struct {
			ReservedMicrosUSD  *int64
			CommittedMicrosUSD *int64
			InputTokens        *int64
			OutputTokens       *int64
			UnknownAttempts    *int64
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return value, errors.Join(ErrProjectionCorrupt, err)
		}
		if fields.ReservedMicrosUSD == nil || fields.CommittedMicrosUSD == nil || fields.InputTokens == nil || fields.OutputTokens == nil || fields.UnknownAttempts == nil {
			return value, ErrProjectionCorrupt
		}
		decoded := Balance{*fields.ReservedMicrosUSD, *fields.CommittedMicrosUSD, *fields.InputTokens, *fields.OutputTokens, *fields.UnknownAttempts}
		if decoded.ReservedMicrosUSD < 0 || decoded.CommittedMicrosUSD < 0 || decoded.InputTokens < 0 || decoded.OutputTokens < 0 || decoded.UnknownAttempts < 0 {
			return value, ErrProjectionCorrupt
		}
		*balance = decoded
		return value, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		var zero T
		return zero, errors.Join(ErrProjectionCorrupt, err)
	}
	return value, nil
}

func validateProjectedGovernanceIdempotency(v GovernanceIdempotency, projectID, operation, keyHash string) error {
	if v.ProjectID != projectID || v.Operation != operation || v.KeyHash != keyHash || !domain.ValidSHA256Label(v.RequestFingerprint) || !domain.ValidWorkUnitID(v.WorkUnitID) || v.RunID != "" && !domain.ValidRunID(v.RunID) {
		return ErrProjectionCorrupt
	}
	return nil
}
