package usage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// Proofs contain only current configuration targets, never historical ID sets.
// The join still requires both a successful finalized request and a successful
// attempt. CompletedAt is compared by time, independently of arrival sequence.
type OnboardingTargets struct{ Providers, Deployments, Routes []string }
type OnboardingEvidence struct {
	Head                           ledger.Watermark
	Metrics                        Metrics
	Providers, Deployments, Routes map[string]time.Time
	LastRequest                    *RequestSummary
	LastAttempt                    *AttemptEvent
	Scanned, InputBytes            int
}
type DiskOnboardingLimits struct {
	Rows, InputBytes, OutputBytes, Targets int
	MaxDuration                            time.Duration
}

// ReadOnboarding keeps a coherent mutable cut across short 160-row transactions.
// Source work is capped for the whole job. Exhaustion returns no partial proof;
// it cannot silently declare an unscanned target unverified. Apply and physical
// Close wait for the admitted job, including gaps between its transactions.
func (p *DiskProjection) ReadOnboarding(ctx context.Context, targets OnboardingTargets, limits DiskOnboardingLimits) (OnboardingEvidence, error) {
	if limits.Rows < 1 || limits.Rows > 65536 || limits.InputBytes < 1 || limits.InputBytes > 128<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 8<<20 || limits.Targets < 1 || limits.Targets > 4096 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return OnboardingEvidence{}, ErrDiskUsageLimit
	}
	count, width := 0, 0
	for _, ids := range [][]string{targets.Providers, targets.Deployments, targets.Routes} {
		for _, id := range ids {
			count++
			if count > limits.Targets || len(id) > (1<<20)-width {
				return OnboardingEvidence{}, ErrDiskUsageLimit
			}
			width += len(id)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	var result OnboardingEvidence
	err := p.readRows(ctx, func(db *usageEngine) error {
		wanted := func(ids []string) map[string]time.Time {
			m := make(map[string]time.Time, len(ids))
			for _, id := range ids {
				if id != "" {
					m[id] = time.Time{}
				}
			}
			return m
		}
		result.Providers, result.Deployments, result.Routes = wanted(targets.Providers), wanted(targets.Deployments), wanted(targets.Routes)
		budget := diskFailureBudget{limits: DiskUsagePageLimits{ScanRows: limits.Rows, InputBytes: limits.InputBytes}}
		view := func(fn func(*usageTx) error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return wrapUsageDatabase(db).View(fn)
		}
		if err := view(func(tx *usageTx) error {
			state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
			if err != nil || !found || state.Version != diskUsageVersion {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
			result.Head, result.Metrics = state.Watermark, state.Metrics
			summaries, index := tx.Bucket([]byte("summaries")), tx.Bucket([]byte("attempt_index"))
			if summaries == nil || index == nil {
				return ErrDiskUsageCorrupt
			}
			key, _ := summaries.Cursor().Last()
			if key == nil {
				return nil
			}
			if len(key) != 8 {
				return ErrDiskUsageCorrupt
			}
			seq := binary.BigEndian.Uint64(key)
			if seq > result.Head.Sequence {
				return ErrDiskUsageCorrupt
			}
			summary, err := diskFailureSummary(tx, seq, &budget)
			if err != nil {
				return err
			}
			result.LastRequest = &summary
			prefix := diskUsageIndex(1, summary.RequestID, 0)[:33]
			cursor := index.Cursor()
			key, _ = cursor.Seek(diskUsageIndex(1, summary.RequestID, ^uint64(0)))
			if key == nil {
				key, _ = cursor.Last()
			} else if !bytes.Equal(key, diskUsageIndex(1, summary.RequestID, ^uint64(0))) {
				key, _ = cursor.Prev()
			}
			if !bytes.HasPrefix(key, prefix) {
				return nil
			}
			if len(key) != 41 {
				return ErrDiskUsageCorrupt
			}
			seq = binary.BigEndian.Uint64(key[33:])
			if seq > result.Head.Sequence {
				return ErrDiskUsageCorrupt
			}
			attempt, err := diskFailureAttempt(tx, seq, summary.RequestID, &budget)
			if err != nil {
				return err
			}
			result.LastAttempt = &attempt
			return nil
		}); err != nil {
			return err
		}
		update := func(m map[string]time.Time, id string, at time.Time) {
			if newest, exists := m[id]; exists && at.After(newest) {
				m[id] = at
			}
		}
		var after uint64
		for {
			done := false
			if err := view(func(tx *usageTx) error {
				attempts, index := tx.Bucket([]byte("attempts")), tx.Bucket([]byte("summary_request"))
				if attempts == nil || index == nil {
					return ErrDiskUsageCorrupt
				}
				cursor := attempts.Cursor()
				key, raw := cursor.First()
				if after != 0 {
					key, raw = cursor.Seek(diskUsageSequence(after))
					if key != nil && bytes.Equal(key, diskUsageSequence(after)) {
						key, raw = cursor.Next()
					}
				}
				for rows := 0; key != nil && rows < 160; rows++ {
					if err := ctx.Err(); err != nil {
						return err
					}
					if len(key) != 8 {
						return ErrDiskUsageCorrupt
					}
					seq := binary.BigEndian.Uint64(key)
					if seq <= after || seq > result.Head.Sequence {
						return ErrDiskUsageCorrupt
					}
					if err := budget.take(raw, len(key)); err != nil {
						return err
					}
					var a AttemptEvent
					if json.Unmarshal(raw, &a) != nil || a.Sequence != seq || a.RequestID == "" {
						return ErrDiskUsageCorrupt
					}
					after = seq
					if a.Status == "success" {
						_, provider := result.Providers[a.ProviderID]
						_, deployment := result.Deployments[a.DeploymentID]
						_, route := result.Routes[a.RouteID]
						if provider || deployment || route {
							requestKey := index.Get(diskUsageID(a.RequestID))
							if err := budget.take(requestKey, 32); err != nil {
								return err
							}
							if requestKey == nil {
								version, found, err := diskDetailSummary(tx, a.RequestID, result.Head.Sequence, 0, &budget)
								if err != nil {
									return err
								}
								// A final immutable version cannot lose its terminal association
								// and become an unverified pending request. Full key-set deletion
								// still belongs to the authenticated snapshot protocol (D-03).
								if found && version.Summary.Sequence != 0 {
									return ErrDiskUsageCorrupt
								}
							}
							if requestKey != nil {
								if len(requestKey) != 8 {
									return ErrDiskUsageCorrupt
								}
								requestSeq := binary.BigEndian.Uint64(requestKey)
								if requestSeq > result.Head.Sequence {
									return ErrDiskUsageCorrupt
								}
								s, err := diskFailureSummary(tx, requestSeq, &budget)
								if err != nil {
									return err
								}
								if s.RequestID != a.RequestID {
									return ErrDiskUsageCorrupt
								}
								if s.Outcome == "success" {
									update(result.Providers, a.ProviderID, a.CompletedAt)
									update(result.Deployments, a.DeploymentID, a.CompletedAt)
									update(result.Routes, a.RouteID, a.CompletedAt)
								}
							}
						}
					}
					key, raw = cursor.Next()
				}
				done = key == nil
				return nil
			}); err != nil {
				return err
			}
			if done {
				break
			}
		}
		// Absence differs from a zero instant in the readiness comparisons.
		for _, m := range []map[string]time.Time{result.Providers, result.Deployments, result.Routes} {
			for id, at := range m {
				if at.IsZero() {
					delete(m, id)
				}
			}
		}
		result.Scanned, result.InputBytes = budget.rows, budget.bytes
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if len(raw) > limits.OutputBytes {
			return ErrDiskUsageLimit
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errDiskFailureBudget) {
			err = ErrDiskUsageLimit
		}
		return OnboardingEvidence{}, err
	}
	return result, nil
}
