package bolt

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

var ErrPricingPageCursor = errors.New("invalid pricing page cursor or limit")
var ErrPricingPageChanged = errors.New("pricing timeline changed; restart listing")

type DeploymentPriceView struct {
	domain.DeploymentPriceVersion
	Status domain.PriceLifecycleStatus `json:"status"`
}

type DeploymentPricePage struct {
	Items              []DeploymentPriceView `json:"items"`
	NextCursor         string                `json:"next_cursor"`
	ActivePrice        *DeploymentPriceView  `json:"active_price"`
	NextScheduledPrice *DeploymentPriceView  `json:"next_scheduled_price"`
	BlockingPrice      *DeploymentPriceView  `json:"blocking_price"`
	SelectedAt         time.Time             `json:"selected_at"`
	Revision           uint64                `json:"revision"`
}

// The process-private cursor authenticates deployment, revision and the native
// time key. It is a continuation, never authority to accept price content.
// Reopening a Store requires callers to restart; no cursor registry is kept.
const pricePageCursorBytes = 1 + 32 + 8 + 12 + 32

func (s *Store) decodePricingPageCursor(cursor, deployment string) ([]byte, *uint64, error) {
	if cursor == "" {
		return nil, nil, nil
	}
	if len(cursor) > 128 {
		return nil, nil, ErrPricingPageCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) != pricePageCursorBytes || raw[0] != 1 {
		return nil, nil, ErrPricingPageCursor
	}
	dep := sha256.Sum256([]byte(deployment))
	mac := hmac.New(sha256.New, s.pricingPageKey[:])
	mac.Write(raw[:53])
	if !bytes.Equal(raw[1:33], dep[:]) || !hmac.Equal(raw[53:], mac.Sum(nil)) {
		return nil, nil, ErrPricingPageCursor
	}
	revision := binary.BigEndian.Uint64(raw[33:41])
	return raw[41:53], &revision, nil
}

func (s *Store) encodePricingPageCursor(deployment string, revision uint64, key []byte) string {
	var raw [pricePageCursorBytes]byte
	raw[0] = 1
	dep := sha256.Sum256([]byte(deployment))
	copy(raw[1:33], dep[:])
	binary.BigEndian.PutUint64(raw[33:41], revision)
	copy(raw[41:53], key)
	mac := hmac.New(sha256.New, s.pricingPageKey[:])
	mac.Write(raw[:53])
	mac.Sum(raw[:53])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func (s *Store) withAuditedPricingTimeline(ctx context.Context, deployment string, expected *uint64, operation *pricingIOReservation, read func(*Tx, uint64) error) error {
	state := s.deploymentPricingState(deployment)
	state.timelineMu.Lock()
	defer state.timelineMu.Unlock()
	for pass := 0; pass < 2; pass++ {
		err := s.view(func(tx *Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			revision, err := deploymentPriceRevisionTx(tx, deployment)
			if err != nil {
				return err
			}
			if expected != nil && revision != *expected {
				return ErrPricingPageChanged
			}
			if !state.timelineLoaded || state.timelineKey != sha256.Sum256([]byte(deployment)) || revision != state.timelineRevision {
				return ErrPricingRecoveryChanged
			}
			return read(tx, revision)
		})
		if !errors.Is(err, ErrPricingRecoveryChanged) {
			return err
		}
		state.timelineLoaded = false
		if pass == 1 {
			return err
		}
		revision, err := s.auditDeploymentTimelineWithReservation(ctx, deployment, operation)
		if err != nil {
			return err
		}
		state.timelineRevision, state.timelineKey, state.timelineLoaded = revision, sha256.Sum256([]byte(deployment)), true
	}
	return ErrPricingRecoveryChanged
}

func pricingPageRecord(tx *Tx, deployment string, key, id []byte) (domain.DeploymentPriceVersion, int, error) {
	var price domain.DeploymentPriceVersion
	if len(key) != 12 || id == nil {
		return price, 0, domain.ErrPriceTimelineConflict
	}
	raw := tx.Bucket(bucketDeploymentPriceVersions).Get(id)
	if raw == nil {
		return price, 0, errors.New("deployment price timeline references a missing record")
	}
	if len(id) > metadatajournal.MaxPayloadSize || len(raw) > metadatajournal.MaxPayloadSize-len(id) {
		return price, 0, ErrPricingAuditLimit
	}
	if err := json.Unmarshal(raw, &price); err != nil {
		return price, 0, err
	}
	if price.ID != string(id) || price.DeploymentID != deployment || !bytes.Equal(key, deploymentPriceTimelineKey(price.EffectiveFrom)) {
		return price, 0, domain.ErrPriceTimelineConflict
	}
	return price, len(raw) + len(id), price.Validate()
}

func pricingPageView(price domain.DeploymentPriceVersion, selectedAt time.Time, activeID string) DeploymentPriceView {
	status := domain.PriceLifecycleSuperseded
	switch {
	case price.CancelledAt != nil:
		status = domain.PriceLifecycleCancelled
	case price.EffectiveFrom.After(selectedAt):
		status = domain.PriceLifecycleScheduled
	case price.ID == activeID:
		status = domain.PriceLifecycleActive
	}
	return DeploymentPriceView{price, status}
}

// DeploymentPriceVersionsPage reads at most limit rows / 1 MiB from the native
// timeline. One native wide row may exceed the page target within its 8 MiB cap.
// Three summary records and lifecycle rows come from the same read transaction.
// Facts must retain the cursor's revision; statuses are live at this call's time.
func (s *Store) DeploymentPriceVersionsPage(ctx context.Context, deployment, cursor string, limit int, selectedAt time.Time) (page DeploymentPricePage, resultErr error) {
	if deployment == "" || limit < 1 || limit > 100 || selectedAt.IsZero() || selectedAt.Location() != time.UTC {
		return page, ErrPricingPageCursor
	}
	leave, err := s.enter()
	if err != nil {
		return page, err
	}
	defer leave()
	op, err := s.acquirePricingIO()
	if err != nil {
		return page, err
	}
	defer func() { resultErr = errors.Join(resultErr, op.release()) }()
	ctx, cancel := s.confirmationContext(ctx)
	defer cancel()
	s.pricingPageKeyOnce.Do(func() { _, s.pricingPageKeyErr = rand.Read(s.pricingPageKey[:]) })
	if s.pricingPageKeyErr != nil {
		return page, s.pricingPageKeyErr
	}
	after, expected, err := s.decodePricingPageCursor(cursor, deployment)
	if err != nil {
		return page, err
	}
	err = s.withAuditedPricingTimeline(ctx, deployment, expected, op, func(tx *Tx, revision uint64) error {
		page = DeploymentPricePage{Items: make([]DeploymentPriceView, 0), SelectedAt: selectedAt, Revision: revision}
		if tx.Bucket(bucketDeployments).Get([]byte(deployment)) == nil {
			return ErrNotFound
		}
		timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deployment))
		if timeline == nil {
			return nil
		}
		active, err := selectDeploymentPriceVersionTxContext(ctx, tx, deployment, selectedAt)
		if err != nil && !errors.Is(err, domain.ErrPriceUnavailable) {
			return err
		}
		if err == nil {
			v := pricingPageView(active, selectedAt, active.ID)
			page.ActivePrice = &v
		}
		c := timeline.Cursor()
		key, id := c.Seek(deploymentPriceTimelineKey(selectedAt))
		for key != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			p, _, err := pricingPageRecord(tx, deployment, key, id)
			if err != nil {
				return err
			}
			if p.CancelledAt == nil && p.EffectiveFrom.After(selectedAt) {
				v := pricingPageView(p, selectedAt, active.ID)
				page.NextScheduledPrice = &v
				break
			}
			key, id = c.Next()
		}
		key, id = c.Last()
		for key != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			p, _, err := pricingPageRecord(tx, deployment, key, id)
			if err != nil {
				return err
			}
			if !p.EffectiveFrom.After(selectedAt) {
				break
			}
			if p.CancelledAt == nil {
				v := pricingPageView(p, selectedAt, active.ID)
				page.BlockingPrice = &v
				break
			}
			key, id = c.Prev()
		}
		key, id = c.First()
		if after != nil {
			key, id = c.Seek(after)
			if !bytes.Equal(key, after) {
				return ErrPricingPageChanged
			}
			key, id = c.Next()
		}
		total := 0
		var last [12]byte
		for key != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Size is checked before decoding the row that may belong to the next page.
			raw := tx.Bucket(bucketDeploymentPriceVersions).Get(id)
			if len(page.Items) > 0 && (len(page.Items) == limit || len(raw)+len(id) > priceAuditPageBytes-total) {
				page.NextCursor = s.encodePricingPageCursor(deployment, revision, last[:])
				break
			}
			p, size, err := pricingPageRecord(tx, deployment, key, id)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, pricingPageView(p, selectedAt, active.ID))
			total += size
			copy(last[:], key)
			key, id = c.Next()
		}
		return ctx.Err()
	})
	if err != nil {
		return DeploymentPricePage{}, err
	}
	return page, nil
}
