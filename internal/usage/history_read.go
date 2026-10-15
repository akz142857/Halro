package usage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// historyScanBudget bounds how many index entries one page may examine. A list
// filtered on something rare would otherwise read the whole history to
// discover there is nothing more; with the budget it returns what it found and
// a cursor to continue from, so a page costs at most this much whatever the
// size of the history behind it.
var historyScanBudget = 20000

// errStopWalk ends a walk early without reporting a failure.
var errStopWalk = errors.New("stop history walk")

// walkSequencesBackward visits the sequence keys of bucket below before (zero
// means from the newest), newest first.
func walkSequencesBackward(bucket *bbolt.Bucket, before uint64, visit func(sequence uint64, value []byte) error) error {
	cursor := bucket.Cursor()
	var key, value []byte
	if before == 0 {
		key, value = cursor.Last()
	} else {
		key, value = cursor.Seek(sequenceKey(before))
		if key == nil {
			key, value = cursor.Last()
		}
		for key != nil && (len(key) != 8 || binary.BigEndian.Uint64(key) >= before) {
			if len(key) != 8 {
				return errors.New("usage history sequence key is malformed")
			}
			key, value = cursor.Prev()
		}
	}
	for ; key != nil; key, value = cursor.Prev() {
		if len(key) != 8 {
			return errors.New("usage history sequence key is malformed")
		}
		if err := visit(binary.BigEndian.Uint64(key), value); err != nil {
			if errors.Is(err, errStopWalk) {
				return nil
			}
			return err
		}
	}
	return nil
}

// walkPrefixBackward visits the sequences filed under one prefix of a
// composite index (request_attempts, project_attempts), newest first and below
// before.
func walkPrefixBackward(bucket *bbolt.Bucket, prefix string, before uint64, visit func(sequence uint64) error) error {
	start := make([]byte, 0, len(prefix)+1)
	start = append(start, prefix...)
	start = append(start, historyRequestSeparator)
	cursor := bucket.Cursor()
	var key []byte
	if before == 0 {
		// The first key past every sequence of this prefix.
		limit := append(bytes.Clone(start), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		key, _ = cursor.Seek(limit)
	} else {
		key, _ = cursor.Seek(compositeKey(prefix, before))
	}
	if key == nil {
		key, _ = cursor.Last()
	} else {
		key, _ = cursor.Prev()
	}
	for ; key != nil && bytes.HasPrefix(key, start); key, _ = cursor.Prev() {
		// A longer key belongs to another id that begins with this one and
		// the separator; it is skipped, not taken for the end of the prefix.
		if len(key) != len(start)+8 {
			continue
		}
		if err := visit(binary.BigEndian.Uint64(key[len(start):])); err != nil {
			if errors.Is(err, errStopWalk) {
				return nil
			}
			return err
		}
	}
	return nil
}

// walkPrefixForward visits one prefix's sequences oldest first.
func walkPrefixForward(bucket *bbolt.Bucket, prefix string, visit func(sequence uint64) error) error {
	start := make([]byte, 0, len(prefix)+1)
	start = append(start, prefix...)
	start = append(start, historyRequestSeparator)
	cursor := bucket.Cursor()
	for key, _ := cursor.Seek(start); key != nil && bytes.HasPrefix(key, start); key, _ = cursor.Next() {
		if len(key) != len(start)+8 {
			continue
		}
		if err := visit(binary.BigEndian.Uint64(key[len(start):])); err != nil {
			return err
		}
	}
	return nil
}

func (s *HistoryStore) attemptAt(tx *bbolt.Tx, sequence uint64) (AttemptEvent, error) {
	key := sequenceKey(sequence)
	raw := tx.Bucket(historyAttempts).Get(key)
	if raw == nil {
		return AttemptEvent{}, fmt.Errorf("usage history has no attempt at sequence %d", sequence)
	}
	return s.decodeAttempt(key, raw)
}

func (s *HistoryStore) decodeAttempt(key, raw []byte) (AttemptEvent, error) {
	var attempt AttemptEvent
	if err := s.getSealed(historyAttempts, key, raw, &attempt); err != nil {
		return AttemptEvent{}, err
	}
	if attempt.Sequence != binary.BigEndian.Uint64(key) {
		return AttemptEvent{}, s.tampered()
	}
	return attempt, nil
}

func (s *HistoryStore) summaryAt(tx *bbolt.Tx, sequence uint64) (RequestSummary, error) {
	key := sequenceKey(sequence)
	raw := tx.Bucket(historyRequests).Get(key)
	if raw == nil {
		return RequestSummary{}, fmt.Errorf("usage history has no request at sequence %d", sequence)
	}
	var summary RequestSummary
	if err := s.getSealed(historyRequests, key, raw, &summary); err != nil {
		return RequestSummary{}, err
	}
	if summary.Sequence != sequence {
		return RequestSummary{}, s.tampered()
	}
	return summary, nil
}

// requestAttempts is every attempt of one request, oldest first.
func (s *HistoryStore) requestAttempts(tx *bbolt.Tx, requestID string) ([]AttemptEvent, error) {
	var attempts []AttemptEvent
	err := walkPrefixForward(tx.Bucket(historyRequestAttempts), requestID, func(sequence uint64) error {
		attempt, err := s.attemptAt(tx, sequence)
		if err != nil {
			return err
		}
		if attempt.RequestID != requestID {
			return s.tampered()
		}
		attempts = append(attempts, attempt)
		return nil
	})
	return attempts, err
}

// dayRollups visits every stored row of one accounting day.
func (s *HistoryStore) dayRollups(tx *bbolt.Tx, periodID string, timezoneVersion uint64, visit func(domain.RollupKey, domain.DailyRollup) error) error {
	prefix := []byte(domain.RollupDayPrefix(periodID, timezoneVersion))
	cursor := tx.Bucket(historyRollups).Cursor()
	for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
		parsed, err := domain.DecodeRollupKey(string(key))
		if err != nil {
			return err
		}
		var row domain.DailyRollup
		if err := s.getSealed(historyRollups, key, value, &row); err != nil {
			return err
		}
		if err := visit(parsed, row); err != nil {
			return err
		}
	}
	return nil
}

// RollupRange walks the stored daily rows whose accounting date falls in
// [startPeriodID, endPeriodID], in key order. An empty bound is open on that
// side. Period ids are fixed-width local dates, so key order is chronological
// and the walk stops at the first row past the end. The watermark is the
// position the rows describe, read in the same transaction; it is zero for an
// index that has written nothing.
func (s *HistoryStore) RollupRange(startPeriodID, endPeriodID string, visit func(domain.RollupKey, domain.DailyRollup) error) (ledger.Watermark, error) {
	var watermark ledger.Watermark
	err := s.db.View(func(tx *bbolt.Tx) error {
		if raw := tx.Bucket(historyMeta).Get(historyKeyState); raw != nil {
			var state HistoryState
			if err := s.getSealed(historyMeta, historyKeyState, raw, &state); err != nil {
				return err
			}
			watermark = state.Watermark
		}
		cursor := tx.Bucket(historyRollups).Cursor()
		for key, value := cursor.Seek([]byte(startPeriodID)); key != nil; key, value = cursor.Next() {
			parsed, err := domain.DecodeRollupKey(string(key))
			if err != nil {
				return err
			}
			if endPeriodID != "" && parsed.PeriodID > endPeriodID {
				return nil
			}
			var row domain.DailyRollup
			if err := s.getSealed(historyRollups, key, value, &row); err != nil {
				return err
			}
			if err := visit(parsed, row); err != nil {
				return err
			}
		}
		return nil
	})
	return watermark, err
}

// hoursSince reads the hourly rows from since on, oldest first.
func (s *HistoryStore) hoursSince(tx *bbolt.Tx, since time.Time) ([]hourlyRow, error) {
	var rows []hourlyRow
	cursor := tx.Bucket(historyHourly).Cursor()
	for key, value := cursor.Seek(sequenceKey(uint64(since.Unix()))); key != nil; key, value = cursor.Next() {
		var row hourlyRow
		if err := s.getSealed(historyHourly, key, value, &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// OnboardingEvidence is what the onboarding checklist reads from usage: when
// each deployment, route and provider last served a request that succeeded,
// and the most recent finished request with its last attempt.
type OnboardingEvidence struct {
	Deployments map[string]time.Time
	Routes      map[string]time.Time
	Providers   map[string]time.Time
	Latest      *RequestSummary
	// LatestAttempt is the final attempt of Latest, when it had one.
	LatestAttempt *AttemptEvent
}

func (a *Aggregate) OnboardingEvidence() (OnboardingEvidence, error) {
	evidence := OnboardingEvidence{
		Deployments: map[string]time.Time{}, Routes: map[string]time.Time{}, Providers: map[string]time.Time{},
	}
	if err := a.Flush(); err != nil {
		return evidence, err
	}
	s := a.history
	err := s.db.View(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(historySuccesses).ForEach(func(key, value []byte) error {
			var at time.Time
			if err := s.getSealed(historySuccesses, key, value, &at); err != nil {
				return err
			}
			name := string(key)
			switch {
			case len(name) > 2 && name[:2] == successDeployment:
				evidence.Deployments[name[2:]] = at
			case len(name) > 2 && name[:2] == successRoute:
				evidence.Routes[name[2:]] = at
			case len(name) > 2 && name[:2] == successProvider:
				evidence.Providers[name[2:]] = at
			}
			return nil
		}); err != nil {
			return err
		}
		key, value := tx.Bucket(historyRequests).Cursor().Last()
		if key == nil {
			return nil
		}
		var latest RequestSummary
		if err := s.getSealed(historyRequests, key, value, &latest); err != nil {
			return err
		}
		evidence.Latest = &latest
		attempts, err := s.requestAttempts(tx, latest.RequestID)
		if err != nil {
			return err
		}
		if len(attempts) > 0 {
			evidence.LatestAttempt = &attempts[len(attempts)-1]
		}
		return nil
	})
	return evidence, err
}

// AttemptsAfter visits every stored attempt past after, in ledger order. The
// Parquet export reads its increment this way rather than from a copy of the
// whole history.
func (s *HistoryStore) AttemptsAfter(after uint64, visit func(AttemptEvent) error) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(historyAttempts).Cursor()
		for key, value := cursor.Seek(sequenceKey(after + 1)); key != nil; key, value = cursor.Next() {
			attempt, err := s.decodeAttempt(key, value)
			if err != nil {
				return err
			}
			if err := visit(attempt); err != nil {
				return err
			}
		}
		return nil
	})
}

// RequestsAfter visits every stored request summary finished past after, in
// ledger order.
func (s *HistoryStore) RequestsAfter(after uint64, visit func(RequestSummary) error) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(historyRequests).Cursor()
		for key, value := cursor.Seek(sequenceKey(after + 1)); key != nil; key, value = cursor.Next() {
			var summary RequestSummary
			if err := s.getSealed(historyRequests, key, value, &summary); err != nil {
				return err
			}
			if err := visit(summary); err != nil {
				return err
			}
		}
		return nil
	})
}

// IsHistoryEmpty reports whether err only says the index has written nothing.
func IsHistoryEmpty(err error) bool {
	return errors.Is(err, errHistoryStateMissing)
}

// State is the Ledger position the stored rows describe, with the counters
// committed alongside it.
func (s *HistoryStore) State() (HistoryState, error) {
	var state HistoryState
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(historyMeta).Get(historyKeyState)
		if raw == nil {
			return errHistoryStateMissing
		}
		return s.getSealed(historyMeta, historyKeyState, raw, &state)
	})
	return state, err
}
