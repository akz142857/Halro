package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/redaction"
)

type resourceSubmission struct {
	record  *domain.ProviderResource
	outcome string
	phase   string
}

// submitAccountedResource pins one attempt for the job. The dispatch ID is
// recorded before network I/O and the upstream ID before handing the job back.
func (s *Service) submitAccountedResource(ctx context.Context, principal auth.AuthResult, target provider.Target, operation provider.Operation, record domain.ProviderResource, input, output int64, invoke func(string) (string, string, error)) error {
	active := &resourceSubmission{}
	s.resourceSubmissions.Store(record.ID, active)
	defer s.resourceSubmissions.CompareAndDelete(record.ID, active)
	target.ProviderResourceID = record.ID
	total, err := addTokens(input, output)
	if err != nil {
		s.releaseUnusedJobReservation(ctx, record)
		return gatewayError("token_limit_exceeded", "job token bound is too large", 400, err)
	}
	run, err := s.beginRequestRun(ctx, principal, record.PublicModel, []provider.Target{target}, total, input, output, nil, admitFullRequest)
	if err != nil {
		s.releaseUnusedJobReservation(ctx, record)
		return err
	}
	defer run.close()
	attempt, err := s.startAttempt(ctx, run, target, operation, input, output, 0, 0, 1)
	if err != nil {
		s.releaseUnusedJobReservation(ctx, record)
		return s.exhaustedAttemptsError(err)
	}
	record.AttemptID = attempt.accounting.AttemptID
	abort := func(outcome string) error {
		attempt.concurrency.Release()
		attempt.gate.Abandon()
		run.finalized = true
		return s.recordRefusedResource(ctx, active, record, outcome, phasePreProvider)
	}
	if attempt.accounting.PriceSnapshot == nil {
		return gatewayError("price_unavailable", "an accounting price snapshot is required for asynchronous jobs", 409, abort("price_unavailable"))
	}
	record, err = s.markInFlight(ctx, record)
	if err != nil {
		return errors.Join(err, abort("accounting_error"))
	}
	upstreamID, status, providerErr := invoke(run.requestID)
	if providerErr == nil && (upstreamID == "" || status == "") {
		providerErr = &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "accepted job has no usable upstream identity"}
	}
	attempt.concurrency.Release()
	attempt.logProviderFailure(providerErr)
	attempt.reportGate(providerErr)
	run.finalized = true // The resource settlement finalizes the submitting request.
	run.recordProviderResult(providerErr, budget.Settlement{ProviderInputTokens: input, ProviderOutputTokens: output})

	if providerErr != nil {
		var classified *provider.Error
		ambiguous := !errors.As(providerErr, &classified) || classified.Ambiguous
		if !ambiguous {
			if err := s.recordRefusedResource(ctx, active, record, "provider_error", ""); err != nil {
				return gatewayError("accounting_unavailable", "the job refusal could not be finalized", 503, err)
			}
			return mapProviderError(providerErr)
		}
		record.CreationStatus = creationUnknown
		record.UpdatedAt = s.now()
		_, storeErr := s.writeJobResource(ctx, record)
		if storeErr != nil {
			s.resourceSubmissions.CompareAndSwap(record.ID, active, &resourceSubmission{record: &record})
		}
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		settlementErr := s.settleEstimatedResource(settleCtx, record, attempt.accounting, "provider_error")
		if joined := errors.Join(settlementErr, storeErr); joined != nil {
			return gatewayError("accounting_unavailable", "the job outcome could not be finalized", 503, joined)
		}
		return mapProviderError(providerErr)
	}
	record.UpstreamID, record.CreationStatus, record.Status = upstreamID, creationCompleted, status
	record.UpdatedAt = s.now()
	if _, err := s.writeJobResource(ctx, record); err != nil {
		// A failed replication confirmation does not prove that the local
		// write failed. Retain the accepted ID for maintenance to recover.
		s.resourceSubmissions.CompareAndSwap(record.ID, active, &resourceSubmission{record: &record})
		return gatewayError("resource_store_unavailable", "upstream job identity could not be recorded", 503, err)
	}
	return nil
}

func (s *Service) writeJobResource(ctx context.Context, record domain.ProviderResource) (domain.ProviderResource, error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return s.resources.PutProviderResource(writeCtx, record, record.Revision)
}

func (s *Service) releaseUnusedJobReservation(ctx context.Context, record domain.ProviderResource) {
	record.CreationStatus, record.ReservedBy, record.AttemptID = creationReserved, "", ""
	record.UpdatedAt = s.now()
	if _, err := s.writeJobResource(ctx, record); err != nil {
		s.logger.Warn("unused job idempotency reservation could not be released", "resource_id", record.ID, "error", err)
	}
}

func (s *Service) recordRefusedResource(ctx context.Context, active *resourceSubmission, record domain.ProviderResource, outcome, phase string) error {
	record.CreationStatus = creationReserved
	refusal := &resourceSubmission{record: &record, outcome: outcome, phase: phase}
	if err := s.finishRefusedResource(ctx, refusal); err != nil {
		// Preserve proof of refusal in this process even when the metadata
		// write did not land. A live reaper can retry both writes safely.
		s.resourceSubmissions.CompareAndSwap(record.ID, active, refusal)
		return err
	}
	return nil
}

func (s *Service) finishRefusedResource(ctx context.Context, refusal *resourceSubmission) error {
	refused := *refusal.record
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	current, err := s.resources.ProviderResource(readCtx, refused.ProjectID, refused.ID)
	cancel()
	if err != nil {
		return err
	}
	if current.AttemptID != refused.AttemptID && (current.AttemptID != "" || current.Revision != refused.Revision) {
		// The key was already released or reclaimed. A stale repair must not
		// overwrite the next submission's record.
		return nil
	}
	// Record proof that the provider did not run the job before settling.
	// Startup will use the reserved disposition to release the hold at zero.
	if current.CreationStatus != creationReserved {
		current.CreationStatus, current.UpdatedAt = creationReserved, s.now()
		current, err = s.writeJobResource(ctx, current)
		if err != nil {
			return err
		}
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	err = s.accounting.SettleResource(settleCtx, refused, budget.Settlement{Outcome: refusal.outcome, FailurePhase: refusal.phase})
	cancel()
	if err != nil {
		return err
	}
	current.ReservedBy, current.AttemptID, current.UpdatedAt = "", "", s.now()
	_, err = s.writeJobResource(ctx, current)
	return err
}

func (s *Service) retryResourceSubmission(ctx context.Context, submission *resourceSubmission) error {
	desired := *submission.record
	if desired.CreationStatus == creationReserved {
		return s.finishRefusedResource(ctx, submission)
	}
	current, err := s.resources.ProviderResource(ctx, desired.ProjectID, desired.ID)
	if err != nil {
		return err
	}
	if current.AttemptID != desired.AttemptID || current.CreationStatus == desired.CreationStatus {
		return nil
	}
	desired.Revision = current.Revision
	_, err = s.writeJobResource(ctx, desired)
	return err
}

func (s *Service) settleEstimatedResource(ctx context.Context, resource domain.ProviderResource, attempt budget.Attempt, outcome string) error {
	tokens := domain.BilledTokens{Input: attempt.PreparedInputTokens, Output: attempt.PreparedOutputTokens}
	if resource.Kind == domain.ResourceBatch {
		tokens = attempt.PriceSnapshot.ConservativeTokens(tokens.Input, tokens.Output)
	}
	return s.settleResourceUsage(ctx, resource, attempt, tokens, budget.Settlement{
		CostEstimated: true, TokenEstimated: true, Outcome: outcome,
	})
}

func (s *Service) settleResourceUsage(ctx context.Context, resource domain.ProviderResource, attempt budget.Attempt, tokens domain.BilledTokens, settlement budget.Settlement) error {
	settlement.ProviderInputTokens = tokens.Input
	settlement.ProviderOutputTokens = tokens.Output
	settlement.ProviderCachedInputTokens = tokens.CachedInput
	settlement.ProviderCacheWriteInputTokens = tokens.CacheWrite
	settlement.ProviderCacheWriteLongInputTokens = tokens.CacheWriteLong
	settlement.PreparedOutputTokens = attempt.PreparedOutputTokens
	if resource.Kind == domain.ResourceAsyncInvoke {
		settlement.VideoSeconds, settlement.ProviderOutputTokens = tokens.Output, 0
		settlement.TokenEstimated = false
	}
	if attempt.LeaseMode != ledger.LeaseModeUnknownAllowed {
		cost, err := attempt.PriceSnapshot.CalculateAttempt(tokens, attempt.FixedFeeExempt)
		if err != nil {
			return fmt.Errorf("calculate frozen job charge: %w", err)
		}
		settlement.CommittedMicrosUSD = cost.TotalCostMicrosUSD
	}
	return s.accounting.SettleResource(ctx, resource, settlement)
}

func (s *Service) settleAsyncStatus(ctx context.Context, resource domain.ProviderResource, status string) error {
	attempt, pending := s.accounting.PendingResourceAttempt(resource)
	if !pending {
		return nil
	}
	switch status {
	case "Completed":
		return s.settleEstimatedResource(ctx, resource, attempt, "success")
	case "Failed":
		// The status API carries neither a charge nor the billable duration for
		// a failed job. Keep the bounded estimate when execution was accepted.
		return s.settleEstimatedResource(ctx, resource, attempt, "provider_error")
	default:
		return nil
	}
}

func (s *Service) settleExpiredResource(ctx context.Context, resource domain.ProviderResource) error {
	attempt, pending := s.accounting.PendingResourceAttempt(resource)
	if !pending {
		return nil
	}
	return s.settleEstimatedResource(ctx, resource, attempt, "resource_usage_unavailable")
}

func validateVideoQuote(snapshot domain.PriceSnapshot, target provider.Target) error {
	if target.ProviderResourceID != "" && target.ProfileID == domain.ProfileBedrockAsyncNovaReel &&
		snapshot.BillingMode == domain.BillingModeMetered &&
		snapshot.Tier().OutputMicrosPerMillion == 0 && snapshot.Tier().FixedRequestMicrosUSD == 0 {
		return gatewayError("price_unavailable", "async video requires a duration rate or a fixed invocation fee", http.StatusConflict, nil)
	}
	return nil
}

// A linked job's status must remain readable even when its hold consumes the
// last of the project's headroom. Polls still acquire the project and upstream
// request slots. Legacy resources retain their existing synthetic input unit.
func (s *Service) readAccountedJob(ctx context.Context, principal auth.AuthResult, target provider.Target, resource domain.ProviderResource, operation provider.Operation, requestID *string, read func() error) error {
	if resource.AttemptID == "" {
		return s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, operation, 1, requestID, read)
	}
	lease, err := s.limiter.Acquire(principal.Project, 0, s.now())
	if err != nil {
		return s.mapLimitError(err)
	}
	defer lease.Release()
	upstream, err := s.acquireTargetConcurrency(target)
	if err != nil {
		return s.exhaustedAttemptsError(err)
	}
	defer upstream.Release()
	*requestID, err = id.New("req")
	if err != nil {
		return err
	}
	err = read()
	if errors.Is(err, redaction.ErrPolicyRejected) {
		return gatewayError("sensitive_data_detected", "response contains material rejected by policy", 502, err)
	}
	if err != nil {
		return mapProviderError(err)
	}
	return nil
}

// ReconcileProviderResourceBilling uses the original owner binding for
// accepted jobs even when a client never polls. Once the resource retention
// period runs out, missing usage falls back to the frozen reservation quote.
func (s *Service) ReconcileProviderResourceBilling(ctx context.Context) {
	s.resourceSubmissions.Range(func(key, value any) bool {
		if ctx.Err() != nil {
			return false
		}
		submission := value.(*resourceSubmission)
		if submission.record != nil {
			if err := s.retryResourceSubmission(ctx, submission); err != nil {
				s.logger.Warn("provider job submission outcome could not be recovered", "resource_id", key, "error", err)
			} else {
				s.resourceSubmissions.CompareAndDelete(key, submission)
			}
		}
		return true
	})
	for _, lease := range s.accounting.PendingResourceLeases() {
		if ctx.Err() != nil {
			return
		}
		resource, err := s.resources.ProviderResource(ctx, lease.ProjectID, lease.ProviderResourceID)
		if err != nil || resource.AttemptID != lease.AttemptID {
			continue
		}
		if _, tracked := s.resourceSubmissions.Load(resource.ID); tracked {
			continue
		}
		jobCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		switch resource.CreationStatus {
		case creationCompleted:
			err = s.reconcileProviderJob(jobCtx, resource)
		case creationReserved:
			err = s.finishRefusedResource(jobCtx, &resourceSubmission{record: &resource, outcome: "provider_error"})
		case creationInFlight:
			resource.CreationStatus, resource.UpdatedAt = creationUnknown, s.now()
			_, err = s.writeJobResource(jobCtx, resource)
			if err != nil {
				break
			}
			fallthrough
		case creationUnknown:
			err = s.settleUnconfirmedResource(jobCtx, resource)
		}
		cancel()
		if err != nil {
			s.logger.Warn("provider job billing could not be reconciled", "resource_id", resource.ID, "error", err)
		}
	}
}

func (s *Service) settleUnconfirmedResource(ctx context.Context, resource domain.ProviderResource) error {
	attempt, pending := s.accounting.PendingResourceAttempt(resource)
	if !pending {
		return nil
	}
	return s.settleEstimatedResource(ctx, resource, attempt, "provider_error")
}

func (s *Service) reconcileProviderJob(ctx context.Context, resource domain.ProviderResource) error {
	target, err := s.ownedTarget(resource)
	if err != nil {
		if !resource.ExpiresAt.After(s.now()) {
			return s.settleExpiredResource(ctx, resource)
		}
		return err
	}
	requestID, err := id.New("req")
	if err != nil {
		return err
	}
	switch resource.Kind {
	case domain.ResourceBatch:
		adapter, ok := target.Adapter.(provider.ResourceBatchesAdapter)
		if !ok {
			return errors.New("batch owner is unavailable for billing")
		}
		result, getErr := adapter.GetBatch(ctx, requestID, resource.UpstreamID)
		if getErr == nil {
			getErr = s.settleBatchStatus(ctx, resource, target, result, nil)
		}
		err = getErr
		if err == nil && terminalBatchStatus(result.Status) {
			return s.updateResourceStatus(ctx, resource, result.Status)
		}
	case domain.ResourceAsyncInvoke:
		adapter, ok := target.Adapter.(provider.BedrockInferenceResourcesAdapter)
		if !ok {
			return errors.New("async owner is unavailable for billing")
		}
		result, getErr := adapter.GetAsyncInvoke(ctx, requestID, resource.UpstreamID)
		if getErr == nil {
			getErr = s.settleAsyncStatus(ctx, resource, result.Status)
		}
		err = getErr
		if err == nil && (result.Status == "Completed" || result.Status == "Failed") {
			return s.updateResourceStatus(ctx, resource, result.Status)
		}
	}
	if !resource.ExpiresAt.After(s.now()) {
		// Use a fresh context when an upstream timeout used the job's deadline.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		return s.settleExpiredResource(writeCtx, resource)
	}
	return err
}
