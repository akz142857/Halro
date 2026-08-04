package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

const maxPriceProposalsPerDeployment = 10_000

func (s *Store) CreateDeploymentPriceProposal(ctx context.Context, proposal domain.DeploymentPriceProposal, intent domain.PricingAuditIntent, keySHA256 string) (domain.DeploymentPriceProposal, domain.PricingAuditIntent, bool, error) {
	if err := ctx.Err(); err != nil {
		return proposal, intent, false, err
	}
	if err := proposal.Validate(); err != nil {
		return proposal, intent, false, err
	}
	if err := intent.Validate(); err != nil {
		return proposal, intent, false, err
	}
	if intent.Action != "deployment_price.proposal_create" || intent.TargetID != proposal.ID || !validSHA256Label(keySHA256) {
		return proposal, intent, false, errors.New("invalid idempotent pricing proposal mutation")
	}
	replayed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		idempotency := tx.Bucket(bucketPricingProposalIdempotency)
		if raw := idempotency.Get([]byte(keySHA256)); raw != nil {
			var previous pricingIdempotencyRecord
			if err := json.Unmarshal(raw, &previous); err != nil {
				return err
			}
			if previous.RequestSHA256 != intent.RequestSHA256 {
				return ErrIdempotencyConflict
			}
			proposalRaw := tx.Bucket(bucketDeploymentPriceProposals).Get([]byte(previous.PriceID))
			intentRaw := tx.Bucket(bucketPricingAuditIntents).Get([]byte(previous.AuditEventID))
			if proposalRaw == nil || intentRaw == nil {
				return errors.New("pricing proposal idempotency record references missing state")
			}
			if err := json.Unmarshal(proposalRaw, &proposal); err != nil {
				return err
			}
			if err := json.Unmarshal(intentRaw, &intent); err != nil {
				return err
			}
			replayed = true
			return nil
		}
		var deployment domain.Deployment
		deploymentRaw := tx.Bucket(bucketDeployments).Get([]byte(proposal.DeploymentID))
		if deploymentRaw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(deploymentRaw, &deployment); err != nil {
			return err
		}
		if deployment.ProviderID != proposal.ProviderID || deployment.ProviderModel != proposal.ProviderModel || deployment.Region != proposal.Region {
			return errors.New("price proposal does not match deployment identity")
		}
		bucket := tx.Bucket(bucketDeploymentPriceProposals)
		count := 0
		if err := bucket.ForEach(func(_, raw []byte) error {
			var item domain.DeploymentPriceProposal
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if item.DeploymentID == proposal.DeploymentID {
				count++
			}
			return nil
		}); err != nil {
			return err
		}
		if count >= maxPriceProposalsPerDeployment {
			return errors.New("deployment price proposal retention capacity exceeded")
		}
		if bucket.Get([]byte(proposal.ID)) != nil {
			return ErrAlreadyExists
		}
		encoded, err := json.Marshal(proposal)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(proposal.ID), encoded); err != nil {
			return err
		}
		if err := putPricingAuditIntentTx(tx, intent); err != nil {
			return err
		}
		record, err := json.Marshal(pricingIdempotencyRecord{KeySHA256: keySHA256, RequestSHA256: intent.RequestSHA256, PriceID: proposal.ID, AuditEventID: intent.EventID, CreatedAt: intent.OccurredAt})
		if err != nil {
			return err
		}
		if idempotency.Stats().KeyN >= maxPricingIdempotencyRecords {
			return errors.New("pricing proposal idempotency retention capacity exceeded")
		}
		return idempotency.Put([]byte(keySHA256), record)
	})
	return proposal, intent, replayed, err
}

func (s *Store) ListDeploymentPriceProposals(ctx context.Context, deploymentID string) ([]domain.DeploymentPriceProposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var proposals []domain.DeploymentPriceProposal
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeploymentPriceProposals).ForEach(func(_, raw []byte) error {
			var proposal domain.DeploymentPriceProposal
			if err := json.Unmarshal(raw, &proposal); err != nil {
				return err
			}
			if proposal.DeploymentID == deploymentID {
				proposals = append(proposals, proposal)
			}
			return nil
		})
	})
	sort.Slice(proposals, func(i, j int) bool { return proposals[i].CreatedAt.After(proposals[j].CreatedAt) })
	return proposals, err
}

func (s *Store) AdoptDeploymentPriceProposal(ctx context.Context, deploymentID, proposalID, priceID, actor string, effectiveFrom, now time.Time, expectedRevision uint64, intent domain.PricingAuditIntent) (domain.DeploymentPriceProposal, domain.DeploymentPriceVersion, error) {
	var proposal domain.DeploymentPriceProposal
	var price domain.DeploymentPriceVersion
	if err := intent.Validate(); err != nil || intent.Action != "deployment_price.proposal_adopt" || intent.TargetID != proposalID {
		return proposal, price, errors.New("invalid proposal adoption audit intent")
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPriceProposals).Get([]byte(proposalID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &proposal); err != nil {
			return err
		}
		if proposal.DeploymentID != deploymentID {
			return ErrNotFound
		}
		if proposal.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if proposal.Status != domain.PriceProposalPending || !now.Before(proposal.ExpiresAt) || proposal.Match == domain.PriceProposalMatchAmbiguous {
			return domain.ErrPriceVersionUnavailable
		}
		var deployment domain.Deployment
		deploymentRaw := tx.Bucket(bucketDeployments).Get([]byte(proposal.DeploymentID))
		if deploymentRaw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(deploymentRaw, &deployment); err != nil {
			return err
		}
		if deployment.ProviderID != proposal.ProviderID || deployment.ProviderModel != proposal.ProviderModel || deployment.Region != proposal.Region {
			return errors.New("deployment identity changed since proposal creation")
		}
		var current uint64
		if raw := tx.Bucket(bucketDeploymentPriceNext).Get([]byte(proposal.DeploymentID)); raw != nil {
			if len(raw) != 8 {
				return errors.New("invalid deployment price next-version state")
			}
			current = binary.BigEndian.Uint64(raw)
		}
		price = domain.DeploymentPriceVersion{
			ID: priceID, DeploymentID: proposal.DeploymentID, Version: current + 1,
			BillingMode: proposal.BillingMode, Currency: proposal.Currency, FormulaVersion: proposal.FormulaVersion,
			InputMicrosPerMillion: proposal.InputMicrosPerMillion, OutputMicrosPerMillion: proposal.OutputMicrosPerMillion,
			FixedRequestMicrosUSD: proposal.FixedRequestMicrosUSD, EffectiveFrom: effectiveFrom.UTC(), Source: proposal.Source,
			CreatedBy: actor, CreatedAt: now.UTC(), Revision: 1,
		}
		latest, latestErr := selectLatestNonCancelledPriceTx(tx, proposal.DeploymentID)
		if latestErr != nil && !errors.Is(latestErr, domain.ErrPriceUnavailable) {
			return latestErr
		}
		if latest.ID != "" && !price.EffectiveFrom.After(latest.EffectiveFrom) {
			return fmt.Errorf("%w: effective_from must follow all non-cancelled versions", domain.ErrPriceTimelineConflict)
		}
		if err := putDeploymentPriceVersionTx(tx, price); err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPriceNext).Put([]byte(proposal.DeploymentID), versionKey(price.Version)); err != nil {
			return err
		}
		proposal.Status, proposal.AdoptedPriceVersionID, proposal.ReviewedBy = domain.PriceProposalAdopted, price.ID, actor
		proposal.ReviewedAt, proposal.Revision = &now, proposal.Revision+1
		if err := proposal.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(proposal)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPriceProposals).Put([]byte(proposal.ID), encoded); err != nil {
			return err
		}
		effective := price.EffectiveFrom.UTC()
		intent.DeploymentID, intent.PriceVersion, intent.EffectiveFrom = price.DeploymentID, price.Version, &effective
		intent.SourceType, intent.SourceContentSHA256 = price.Source.Type, price.Source.ContentSHA256
		intent.ChangeSummary = fmt.Sprintf("before={proposal:%s} after={price:%s,billing:%s,input:%d,output:%d,fixed:%d}", proposal.ID, price.ID, price.BillingMode, price.InputMicrosPerMillion, price.OutputMicrosPerMillion, price.FixedRequestMicrosUSD)
		return putPricingAuditIntentTx(tx, intent)
	})
	return proposal, price, err
}

func (s *Store) RejectDeploymentPriceProposal(ctx context.Context, deploymentID, proposalID, actor string, now time.Time, expectedRevision uint64, intent domain.PricingAuditIntent) (domain.DeploymentPriceProposal, error) {
	var proposal domain.DeploymentPriceProposal
	if err := intent.Validate(); err != nil || intent.Action != "deployment_price.proposal_reject" || intent.TargetID != proposalID {
		return proposal, errors.New("invalid proposal rejection audit intent")
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPriceProposals)
		raw := bucket.Get([]byte(proposalID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &proposal); err != nil {
			return err
		}
		if proposal.DeploymentID != deploymentID {
			return ErrNotFound
		}
		if proposal.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if proposal.Status != domain.PriceProposalPending {
			return domain.ErrPriceVersionUnavailable
		}
		now = now.UTC()
		proposal.Status, proposal.ReviewedBy, proposal.ReviewedAt, proposal.Revision = domain.PriceProposalRejected, actor, &now, proposal.Revision+1
		if err := proposal.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(proposal)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(proposal.ID), encoded); err != nil {
			return err
		}
		return putPricingAuditIntentTx(tx, intent)
	})
	return proposal, err
}

func selectLatestNonCancelledPriceTx(tx *bbolt.Tx, deploymentID string) (domain.DeploymentPriceVersion, error) {
	var latest domain.DeploymentPriceVersion
	timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deploymentID))
	if timeline == nil {
		return latest, domain.ErrPriceUnavailable
	}
	err := timeline.ForEach(func(_, priceID []byte) error {
		var price domain.DeploymentPriceVersion
		if raw := tx.Bucket(bucketDeploymentPriceVersions).Get(priceID); raw == nil {
			return errors.New("deployment price timeline references a missing record")
		} else if err := json.Unmarshal(raw, &price); err != nil {
			return err
		}
		if price.CancelledAt == nil && (latest.ID == "" || price.EffectiveFrom.After(latest.EffectiveFrom)) {
			latest = price
		}
		return nil
	})
	if err == nil && latest.ID == "" {
		err = domain.ErrPriceUnavailable
	}
	return latest, err
}
