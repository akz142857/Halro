package bolt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func migrateVersionedDeploymentPricing(tx *bbolt.Tx, step func(string) error) error {
	for _, item := range []struct {
		name  []byte
		label string
	}{
		{bucketDeploymentPriceVersions, "deployment_price_versions"},
		{bucketDeploymentPriceTimeline, "deployment_price_timeline"},
		{bucketDeploymentPriceNext, "deployment_price_next_version"},
		{bucketDeploymentPricingHighWater, "deployment_pricing_high_water"},
		{bucketPricingAuditIntents, "pricing_audit_intents"},
		{bucketPricingIdempotency, "pricing_idempotency"},
	} {
		if err := migrationStep(step, "before_create_"+item.label); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(item.name); err != nil {
			return err
		}
		if err := migrationStep(step, "after_create_"+item.label); err != nil {
			return err
		}
	}
	// The legacy-price backfill that used to run here is gone. It could only act
	// on deployments carrying pre-versioned price fields, and since schema 20 a
	// data directory holding deployments is refused rather than upgraded — so
	// the loop could never see one again. Bucket creation above stays: an empty
	// directory still passes through this rung on its way to the current schema.
	//
	// The rung keeps its version and name. Renumbering to close the gap would
	// reuse migration identifiers, which is what makes an upgrade history
	// unambiguous.
	return nil
}

func (s *Store) PricingIdempotencyRequestSHA256(ctx context.Context, keySHA256 string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	var record pricingIdempotencyRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketPricingIdempotency).Get([]byte(keySHA256))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &record)
	})
	return record.RequestSHA256, record.KeySHA256 != "", err
}

func migrateDeployments(tx *bbolt.Tx, step func(string) error) error {
	if err := migrationStep(step, "before_create_deployments_bucket"); err != nil {
		return err
	}
	deployments, err := tx.CreateBucketIfNotExists(bucketDeployments)
	if err != nil {
		return err
	}
	if err := migrationStep(step, "after_create_deployments_bucket"); err != nil {
		return err
	}
	routes := tx.Bucket(bucketRoutes)
	if routes == nil {
		return errors.New("routes bucket is missing")
	}
	type routeMigration struct {
		key        []byte
		route      domain.Route
		deployment domain.Deployment
	}
	var pending []routeMigration
	if err := routes.ForEach(func(key, raw []byte) error {
		if raw == nil {
			return nil
		}
		var route domain.Route
		if err := json.Unmarshal(raw, &route); err != nil {
			return fmt.Errorf("decode legacy route %q: %w", key, err)
		}
		if route.DeploymentID != "" {
			return nil
		}
		if route.ProviderID == "" || route.ProviderModel == "" {
			return fmt.Errorf("legacy route %q cannot be migrated", route.ID)
		}
		deploymentID := "dep_migrated_" + route.ID
		deployment := domain.Deployment{
			ID: deploymentID, Name: route.PublicModel + " / " + route.ProviderModel,
			ProviderID: route.ProviderID, ProviderModel: route.ProviderModel,
			InputMicrosPerMillion:  route.InputMicrosPerMillion,
			OutputMicrosPerMillion: route.OutputMicrosPerMillion,
			Priority:               route.Priority, Weight: 1, Enabled: route.Enabled,
			CreatedAt: route.CreatedAt, UpdatedAt: route.UpdatedAt, Revision: 1,
			DeletedAt: route.DeletedAt,
		}
		route.DeploymentID = deploymentID
		pending = append(pending, routeMigration{
			key: bytes.Clone(key), route: route, deployment: deployment,
		})
		return nil
	}); err != nil {
		return err
	}
	for _, item := range pending {
		encodedDeployment, err := json.Marshal(item.deployment)
		if err != nil {
			return err
		}
		if err := migrationStep(step, "before_put_deployment_"+item.deployment.ID); err != nil {
			return err
		}
		if err := deployments.Put([]byte(item.deployment.ID), encodedDeployment); err != nil {
			return err
		}
		if err := migrationStep(step, "after_put_deployment_"+item.deployment.ID); err != nil {
			return err
		}
		encodedRoute, err := json.Marshal(item.route)
		if err != nil {
			return err
		}
		if err := migrationStep(step, "before_put_route_"+item.route.ID); err != nil {
			return err
		}
		if err := routes.Put(item.key, encodedRoute); err != nil {
			return err
		}
		if err := migrationStep(step, "after_put_route_"+item.route.ID); err != nil {
			return err
		}
	}
	return nil
}

func normalizeDeploymentProfile(deployment *domain.Deployment, instance domain.ProviderInstance, evidence domain.CapabilityEvidence) {
	legacyProfile := deployment.AccessSurface == "" && deployment.ProfileID == ""
	if deployment.AccessSurface == "" {
		deployment.AccessSurface = instance.AccessSurface
	}
	if deployment.ProfileID == "" {
		deployment.ProfileID = instance.ProfileID
	}
	if legacyProfile && !deployment.Capabilities.Chat && !deployment.Capabilities.Embeddings {
		deployment.Capabilities = instance.Capabilities
	}
	if instance.ProfileID == domain.ProfileBedrockConverseText {
		deployment.Capabilities.DeveloperRole = false
	}
	if len(deployment.CapabilityEvidence) == 0 {
		deployment.CapabilityEvidence = domain.EvidenceForCapabilities(deployment.Capabilities, evidence)
	}
	normalizeDeploymentBinding(deployment, instance)
}

func normalizeDeploymentBinding(deployment *domain.Deployment, instance domain.ProviderInstance) {
	if deployment.BindingID != "" {
		return
	}
	for _, binding := range instance.EffectiveProfileBindings() {
		if binding.ProfileID == deployment.ProfileID && binding.AccessSurface == deployment.AccessSurface {
			deployment.BindingID = binding.ID
			return
		}
	}
}

func (s *Store) PutDeployment(ctx context.Context, deployment domain.Deployment, expectedRevision uint64) (domain.Deployment, error) {
	if err := ctx.Err(); err != nil {
		return domain.Deployment{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		rawProvider := tx.Bucket(bucketProviders).Get([]byte(deployment.ProviderID))
		if rawProvider == nil {
			return fmt.Errorf("provider %q: %w", deployment.ProviderID, ErrNotFound)
		}
		var instance domain.ProviderInstance
		if err := json.Unmarshal(rawProvider, &instance); err != nil {
			return fmt.Errorf("decode provider %q: %w", deployment.ProviderID, err)
		}
		if err := deployment.Validate(); err != nil {
			return err
		}
		if err := validateDeploymentProviderProfile(deployment, instance); err != nil {
			return err
		}
		return putVersioned(tx.Bucket(bucketDeployments), deployment.ID, expectedRevision, &deployment)
	})
	return deployment, err
}

func validateDeploymentProviderProfile(deployment domain.Deployment, instance domain.ProviderInstance) error {
	bindingID := deployment.BindingID
	if bindingID == "" {
		bindingID = domain.DefaultProviderProfileBindingID(instance.ID, deployment.ProfileID)
	}
	binding, ok := instance.ProfileBinding(bindingID)
	if !ok {
		return errors.New("deployment provider profile binding was not found")
	}
	if deployment.AccessSurface != binding.AccessSurface || deployment.ProfileID != binding.ProfileID {
		return errors.New("deployment access surface or profile does not match provider")
	}
	if !domain.ProviderCapabilitiesSubset(deployment.Capabilities, binding.Capabilities) {
		return errors.New("deployment capabilities exceed provider capabilities")
	}
	for name, value := range deployment.CapabilityEvidence {
		providerValue, ok := binding.CapabilityEvidence[name]
		if !ok || capabilityEvidenceRank(value) > capabilityEvidenceRank(providerValue) {
			return errors.New("deployment capability evidence exceeds provider evidence")
		}
	}
	return nil
}

func (s *Store) GetDeployment(ctx context.Context, id string) (domain.Deployment, error) {
	var deployment domain.Deployment
	err := s.getJSON(ctx, bucketDeployments, id, &deployment)
	if err == nil && deployment.BindingID == "" {
		if instance, providerErr := s.GetProvider(ctx, deployment.ProviderID); providerErr == nil {
			normalizeDeploymentBinding(&deployment, instance)
		}
	}
	return deployment, err
}

func (s *Store) ListDeployments(ctx context.Context) ([]domain.Deployment, error) {
	providers, err := s.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	providerByID := make(map[string]domain.ProviderInstance, len(providers))
	for _, instance := range providers {
		providerByID[instance.ID] = instance
	}
	var deployments []domain.Deployment
	err = s.listJSON(ctx, bucketDeployments, func(raw []byte) error {
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		if instance, ok := providerByID[deployment.ProviderID]; ok {
			normalizeDeploymentBinding(&deployment, instance)
		}
		deployments = append(deployments, deployment)
		return nil
	})
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].ID < deployments[j].ID })
	return deployments, err
}

func (s *Store) CreateDeploymentPriceVersion(ctx context.Context, price domain.DeploymentPriceVersion) (domain.DeploymentPriceVersion, error) {
	created, _, _, err := s.createDeploymentPriceVersion(ctx, price, nil, "")
	return created, err
}

func (s *Store) CreateDeploymentPriceVersionIdempotent(ctx context.Context, price domain.DeploymentPriceVersion, intent domain.PricingAuditIntent, keySHA256 string) (domain.DeploymentPriceVersion, domain.PricingAuditIntent, bool, error) {
	if err := intent.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, domain.PricingAuditIntent{}, false, err
	}
	if intent.Action != "deployment_price.create" || intent.TargetID != price.ID || !validSHA256Label(keySHA256) {
		return domain.DeploymentPriceVersion{}, domain.PricingAuditIntent{}, false, errors.New("invalid idempotent pricing mutation")
	}
	created, effectiveIntent, replayed, err := s.createDeploymentPriceVersion(ctx, price, &intent, keySHA256)
	if effectiveIntent == nil {
		return created, domain.PricingAuditIntent{}, replayed, err
	}
	return created, *effectiveIntent, replayed, err
}

func (s *Store) createDeploymentPriceVersion(ctx context.Context, price domain.DeploymentPriceVersion, intent *domain.PricingAuditIntent, keySHA256 string) (domain.DeploymentPriceVersion, *domain.PricingAuditIntent, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, nil, false, err
	}
	if strings.TrimSpace(price.ID) == "" || strings.TrimSpace(price.DeploymentID) == "" {
		return domain.DeploymentPriceVersion{}, nil, false, errors.New("price version id and deployment id are required")
	}
	if price.Version != 0 || price.Revision != 0 || price.CancelledAt != nil || price.CancelledBy != "" {
		return domain.DeploymentPriceVersion{}, nil, false, errors.New("new price version must not set version, revision, or cancellation metadata")
	}
	var effectiveIntent *domain.PricingAuditIntent
	replayed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if keySHA256 != "" {
			idempotency := tx.Bucket(bucketPricingIdempotency)
			if raw := idempotency.Get([]byte(keySHA256)); raw != nil {
				var previous pricingIdempotencyRecord
				if err := json.Unmarshal(raw, &previous); err != nil {
					return err
				}
				if intent == nil || previous.RequestSHA256 != intent.RequestSHA256 {
					return ErrIdempotencyConflict
				}
				priceRaw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(previous.PriceID))
				intentRaw := tx.Bucket(bucketPricingAuditIntents).Get([]byte(previous.AuditEventID))
				if priceRaw == nil || intentRaw == nil {
					return errors.New("pricing idempotency record references missing state")
				}
				if err := json.Unmarshal(priceRaw, &price); err != nil {
					return err
				}
				var storedIntent domain.PricingAuditIntent
				if err := json.Unmarshal(intentRaw, &storedIntent); err != nil {
					return err
				}
				effectiveIntent, replayed = &storedIntent, true
				return nil
			}
		}
		if tx.Bucket(bucketDeployments).Get([]byte(price.DeploymentID)) == nil {
			return fmt.Errorf("deployment %q: %w", price.DeploymentID, ErrNotFound)
		}
		prices := tx.Bucket(bucketDeploymentPriceVersions)
		if prices.Get([]byte(price.ID)) != nil {
			return ErrAlreadyExists
		}
		nextVersions := tx.Bucket(bucketDeploymentPriceNext)
		var current uint64
		if raw := nextVersions.Get([]byte(price.DeploymentID)); raw != nil {
			if len(raw) != 8 {
				return errors.New("invalid deployment price next-version state")
			}
			current = binary.BigEndian.Uint64(raw)
		}
		if current == ^uint64(0) {
			return errors.New("deployment price version overflow")
		}
		price.Version = current + 1
		price.Revision = 1
		if err := price.Validate(); err != nil {
			return err
		}
		timelineRoot := tx.Bucket(bucketDeploymentPriceTimeline)
		timeline, err := timelineRoot.CreateBucketIfNotExists([]byte(price.DeploymentID))
		if err != nil {
			return err
		}
		key := deploymentPriceTimelineKey(price.EffectiveFrom)
		if timeline.Get(key) != nil {
			return domain.ErrPriceTimelineConflict
		}
		var latest domain.DeploymentPriceVersion
		if err := timeline.ForEach(func(_, priceID []byte) error {
			if priceID == nil {
				return nil
			}
			raw := prices.Get(priceID)
			if raw == nil {
				return errors.New("deployment price timeline references a missing record")
			}
			var existing domain.DeploymentPriceVersion
			if err := json.Unmarshal(raw, &existing); err != nil {
				return err
			}
			if existing.CancelledAt == nil && (latest.ID == "" || existing.EffectiveFrom.After(latest.EffectiveFrom)) {
				latest = existing
			}
			return nil
		}); err != nil {
			return err
		}
		if latest.ID != "" && !price.EffectiveFrom.After(latest.EffectiveFrom) {
			return fmt.Errorf("%w: effective_from must follow all non-cancelled versions", domain.ErrPriceTimelineConflict)
		}
		versionCount, scheduledCount := 0, 0
		if err := timeline.ForEach(func(_, priceID []byte) error {
			if priceID == nil {
				return nil
			}
			versionCount++
			var existing domain.DeploymentPriceVersion
			if err := json.Unmarshal(prices.Get(priceID), &existing); err != nil {
				return err
			}
			if existing.CancelledAt == nil && existing.EffectiveFrom.After(price.CreatedAt) {
				scheduledCount++
			}
			return nil
		}); err != nil {
			return err
		}
		if versionCount >= maxPriceVersionsPerDeployment || (price.EffectiveFrom.After(price.CreatedAt) && scheduledCount >= maxScheduledPricesPerDeployment) {
			return errors.New("deployment pricing retention capacity exceeded")
		}
		if err := putDeploymentPriceVersionTx(tx, price); err != nil {
			return err
		}
		if err := nextVersions.Put([]byte(price.DeploymentID), versionKey(price.Version)); err != nil {
			return err
		}
		if intent != nil {
			effective := price.EffectiveFrom.UTC()
			intent.DeploymentID, intent.PriceVersion, intent.EffectiveFrom = price.DeploymentID, price.Version, &effective
			intent.SourceType, intent.SourceContentSHA256 = price.Source.Type, price.Source.ContentSHA256
			intent.ChangeSummary = fmt.Sprintf("before=none after={billing:%s,input:%d,output:%d,fixed:%d}", price.BillingMode, price.InputMicrosPerMillion, price.OutputMicrosPerMillion, price.FixedRequestMicrosUSD)
			if err := putPricingAuditIntentTx(tx, *intent); err != nil {
				return err
			}
			copy := *intent
			effectiveIntent = &copy
		}
		if keySHA256 != "" {
			if tx.Bucket(bucketPricingIdempotency).Stats().KeyN >= maxPricingIdempotencyRecords {
				return errors.New("pricing idempotency retention capacity exceeded")
			}
			record := pricingIdempotencyRecord{
				KeySHA256: keySHA256, RequestSHA256: intent.RequestSHA256, PriceID: price.ID,
				AuditEventID: intent.EventID, CreatedAt: intent.OccurredAt,
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			return tx.Bucket(bucketPricingIdempotency).Put([]byte(keySHA256), encoded)
		}
		return nil
	})
	return price, effectiveIntent, replayed, err
}

func (s *Store) GetDeploymentPriceVersion(ctx context.Context, deploymentID, priceID string) (domain.DeploymentPriceVersion, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	var price domain.DeploymentPriceVersion
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(priceID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &price); err != nil {
			return err
		}
		if price.DeploymentID != deploymentID {
			return ErrNotFound
		}
		return price.Validate()
	})
	return price, err
}

func (s *Store) ListDeploymentPriceVersions(ctx context.Context, deploymentID string) ([]domain.DeploymentPriceVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var prices []domain.DeploymentPriceVersion
	err := s.db.View(func(tx *bbolt.Tx) error {
		timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deploymentID))
		if timeline == nil {
			if tx.Bucket(bucketDeployments).Get([]byte(deploymentID)) == nil {
				return ErrNotFound
			}
			return nil
		}
		return timeline.ForEach(func(_, priceID []byte) error {
			if priceID == nil {
				return nil
			}
			raw := tx.Bucket(bucketDeploymentPriceVersions).Get(priceID)
			if raw == nil {
				return errors.New("deployment price timeline references a missing record")
			}
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(raw, &price); err != nil {
				return err
			}
			if price.DeploymentID != deploymentID {
				return errors.New("deployment price timeline references another deployment")
			}
			if err := price.Validate(); err != nil {
				return err
			}
			prices = append(prices, price)
			return nil
		})
	})
	return prices, err
}

func (s *Store) SelectDeploymentPriceVersion(ctx context.Context, deploymentID string, selectedAt time.Time) (domain.DeploymentPriceVersion, error) {
	prices, err := s.ListDeploymentPriceVersions(ctx, deploymentID)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	return domain.SelectDeploymentPriceVersion(prices, deploymentID, selectedAt)
}

func (s *Store) deploymentPricingState(deploymentID string) *deploymentPricingState {
	if value, ok := s.pricingStates.Load(deploymentID); ok {
		return value.(*deploymentPricingState)
	}
	value, _ := s.pricingStates.LoadOrStore(deploymentID, &deploymentPricingState{})
	return value.(*deploymentPricingState)
}

// LockDeploymentPricingShared is taken by price selection and pin preparation.
// It is shared because the invariant the gate exists for is that no timeline
// mutation interleaves between a selection and its durable Lease — not that two
// selections exclude each other. Holding it exclusively for selection made the
// per-deployment attempt rate the Gateway's throughput ceiling; see ADR 0012,
// "Amendment 2026-08-07".
func (s *Store) LockDeploymentPricingShared(deploymentID string) func() {
	state := s.deploymentPricingState(deploymentID)
	state.gate.RLock()
	return state.gate.RUnlock
}

// LockDeploymentPricingExclusive is taken by the Admin operations that mutate a
// deployment's price timeline: creation, scheduled cancellation, restore
// confirmation, and proposal adoption. An exclusive acquirer waits for every
// in-flight shared holder, which is what keeps a scheduled cancellation from
// landing between a selection and the Lease that makes it durable. The cost is
// that these operator actions may now wait behind in-flight attempts, bounded by
// one attempt's durable path.
func (s *Store) LockDeploymentPricingExclusive(deploymentID string) func() {
	state := s.deploymentPricingState(deploymentID)
	state.gate.Lock()
	return state.gate.Unlock
}

// clockAnchor returns the last trusted (selection time, observation instant)
// pair for this deployment. Forward-jump detection needs an in-memory anchor
// because the durable high-water mark only ever moves forward and so cannot
// witness a clock that jumped ahead.
func (state *deploymentPricingState) clockAnchor() (pricingClockObservation, bool) {
	state.clockMu.Lock()
	defer state.clockMu.Unlock()
	return state.clock, state.hasClock
}

// mergeClockAnchor advances the anchor only when the candidate selection time is
// newer. Concurrent selections on one deployment can commit in the reverse of
// their capture order, and a plain assignment would let the anchor walk
// backwards — turning ordinary reordering into a forward-jump verdict on a later
// request. The pair is replaced together: an ObservedAt is only meaningful
// beside the SelectedAt it was captured with.
func (state *deploymentPricingState) mergeClockAnchor(candidate pricingClockObservation) {
	state.clockMu.Lock()
	defer state.clockMu.Unlock()
	if !state.hasClock || candidate.SelectedAt.After(state.clock.SelectedAt) {
		state.clock, state.hasClock = candidate, true
	}
}

func (s *Store) PricingReadiness(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeploymentPricingHighWater).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var highWater domain.DeploymentPricingHighWater
			if err := json.Unmarshal(raw, &highWater); err != nil {
				return err
			}
			if err := highWater.Validate(); err != nil {
				return err
			}
			if highWater.Quarantined {
				return fmt.Errorf("deployment %q: %w", highWater.DeploymentID, ErrPricingQuarantined)
			}
			return nil
		})
	})
}

func (s *Store) PricingQuarantineCount(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeploymentPricingHighWater).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var highWater domain.DeploymentPricingHighWater
			if err := json.Unmarshal(raw, &highWater); err != nil {
				return err
			}
			if highWater.Quarantined {
				count++
			}
			return nil
		})
	})
	return count, err
}

func (s *Store) DeploymentPricingQuarantine(ctx context.Context, deploymentID string) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	var high domain.DeploymentPricingHighWater
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(deploymentID))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &high)
	})
	return high.Quarantined, high.QuarantineReason, err
}

func (s *Store) QuarantineRestoredScheduledPrices(ctx context.Context, backupCreatedAt, restoredAt time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	backupCreatedAt, restoredAt = backupCreatedAt.UTC(), restoredAt.UTC()
	if backupCreatedAt.IsZero() || !restoredAt.After(backupCreatedAt) {
		return 0, nil
	}
	count := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		due := make(map[string]domain.DeploymentPriceVersion)
		if err := tx.Bucket(bucketDeploymentPriceVersions).ForEach(func(_, raw []byte) error {
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(raw, &price); err != nil {
				return err
			}
			if price.EffectiveFrom.After(backupCreatedAt) && !price.EffectiveFrom.After(restoredAt) &&
				(price.CancelledAt == nil || price.CancelledAt.After(backupCreatedAt)) {
				if current, ok := due[price.DeploymentID]; !ok || current.EffectiveFrom.Before(price.EffectiveFrom) {
					due[price.DeploymentID] = price
				}
			}
			return nil
		}); err != nil {
			return err
		}
		highs := tx.Bucket(bucketDeploymentPricingHighWater)
		for deploymentID, price := range due {
			var high domain.DeploymentPricingHighWater
			if raw := highs.Get([]byte(deploymentID)); raw != nil {
				if err := json.Unmarshal(raw, &high); err != nil {
					return err
				}
				if high.Quarantined {
					continue
				}
				high.Quarantined = true
				high.QuarantineReason = "restored_scheduled_price_requires_confirmation"
				high.Revision++
			} else {
				high = domain.DeploymentPricingHighWater{
					DeploymentID: deploymentID, LatestObservedPriceVersionID: price.ID,
					LatestSelectedAt: restoredAt, LatestObservedEffectiveFrom: price.EffectiveFrom,
					Quarantined: true, QuarantineReason: "restored_scheduled_price_requires_confirmation", Revision: 1,
				}
			}
			if err := high.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(high)
			if err != nil {
				return err
			}
			if err := highs.Put([]byte(deploymentID), encoded); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) ConfirmRestoredPricing(ctx context.Context, deploymentID string) error {
	return s.confirmRestoredPricing(ctx, deploymentID, nil)

}

func (s *Store) confirmRestoredPricing(ctx context.Context, deploymentID string, intent *domain.PricingAuditIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPricingHighWater)
		raw := bucket.Get([]byte(deploymentID))
		if raw == nil {
			return ErrNotFound
		}
		var high domain.DeploymentPricingHighWater
		if err := json.Unmarshal(raw, &high); err != nil {
			return err
		}
		if !high.Quarantined || !strings.HasPrefix(high.QuarantineReason, "restored_") {
			return ErrRevisionConflict
		}
		high.Quarantined = false
		high.QuarantineReason = ""
		high.Revision++
		if err := high.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(high)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(deploymentID), encoded); err != nil {
			return err
		}
		if intent != nil {
			return putPricingAuditIntentTx(tx, *intent)
		}
		return nil
	})
}

func (s *Store) PrepareDeploymentPricePin(ctx context.Context, deploymentID, attemptID string, selectedAt time.Time, rollbackTolerance, forwardTolerance time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, err
	}
	selectedAt = selectedAt.UTC()
	if deploymentID == "" || attemptID == "" || selectedAt.IsZero() || rollbackTolerance < 0 || forwardTolerance <= 0 {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, errors.New("deployment, attempt, UTC selection time, and valid clock tolerances are required")
	}
	state := s.deploymentPricingState(deploymentID)
	requestedAt := selectedAt
	var price domain.DeploymentPriceVersion
	var snapshot domain.PriceSnapshot
	var intent domain.PricePinIntent
	var quarantined bool
	var observedAt time.Time
	// outcome carries the expected non-fault results — quarantined, no effective
	// price, a conflicting pin — instead of returning them as transaction errors.
	// Under db.Batch an error aborts the whole shared transaction, evicts this
	// call, and re-runs every unrelated sibling from scratch; and both
	// ErrPricingQuarantined and ErrPriceUnavailable are routine, not exceptional
	// (a quarantined deployment keeps being retried, and a deployment with no
	// price still serves under the unknown-price policy). Only genuine faults are
	// returned as errors, so only they can spoil a batch.
	var outcome error
	err := s.batch(func(tx *bbolt.Tx) error {
		// Everything the caller reads is reset here, not accumulated across
		// runs: this body may execute more than once for a single call, and a
		// re-run happens in a fresh transaction that can observe state another
		// writer committed in between — so it can legitimately take a different
		// branch than the previous run did.
		price, snapshot, intent, quarantined, outcome = domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, false, nil
		selectedAt = requestedAt
		// The clock anchor is read per run for the same reason, so a retry can
		// never decide a forward jump from an observation older than the
		// transaction that ends up committing.
		observedAt = time.Now()
		previousObservation, hasObservation := state.clockAnchor()
		forwardJump := false
		if hasObservation {
			elapsed := observedAt.Sub(previousObservation.ObservedAt)
			if elapsed < 0 {
				elapsed = 0
			}
			forwardJump = selectedAt.After(previousObservation.SelectedAt.Add(elapsed).Add(forwardTolerance))
		}
		highWaterBucket := tx.Bucket(bucketDeploymentPricingHighWater)
		var highWater domain.DeploymentPricingHighWater
		if raw := highWaterBucket.Get([]byte(deploymentID)); raw != nil {
			if err := json.Unmarshal(raw, &highWater); err != nil {
				return err
			}
			if err := highWater.Validate(); err != nil {
				return err
			}
			if highWater.Quarantined {
				outcome = ErrPricingQuarantined
				return nil
			}
			if forwardJump {
				highWater.Quarantined = true
				highWater.QuarantineReason = "wall_clock_forward_jump"
				highWater.Revision++
				encoded, err := json.Marshal(highWater)
				if err != nil {
					return err
				}
				if err := highWaterBucket.Put([]byte(deploymentID), encoded); err != nil {
					return err
				}
				quarantined = true
				return nil
			}
			if selectedAt.Before(highWater.LatestSelectedAt) {
				rollback := highWater.LatestSelectedAt.Sub(selectedAt)
				if rollback > rollbackTolerance {
					highWater.Quarantined = true
					highWater.QuarantineReason = "wall_clock_rollback"
					highWater.Revision++
					encoded, err := json.Marshal(highWater)
					if err != nil {
						return err
					}
					if err := highWaterBucket.Put([]byte(deploymentID), encoded); err != nil {
						return err
					}
					quarantined = true
					return nil
				}
				selectedAt = highWater.LatestSelectedAt
			}
		}
		var err error
		price, err = selectDeploymentPriceVersionTx(tx, deploymentID, selectedAt)
		if errors.Is(err, domain.ErrPriceUnavailable) {
			outcome = err
			return nil
		}
		if err != nil {
			return err
		}
		snapshot, err = domain.NewVersionedPriceSnapshot(price, selectedAt)
		if err != nil {
			return err
		}
		digest, err := snapshot.Digest()
		if err != nil {
			return err
		}
		var deployment domain.Deployment
		if raw := tx.Bucket(bucketDeployments).Get([]byte(deploymentID)); raw == nil {
			outcome = ErrNotFound
			return nil
		} else if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		pins := tx.Bucket(bucketDeploymentPricePins)
		if raw := pins.Get([]byte(attemptID)); raw != nil {
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if intent.DeploymentID != deploymentID || intent.PriceVersionID != price.ID || intent.SnapshotSHA256 != digest {
				outcome = ErrIdempotencyConflict
				return nil
			}
			return nil
		}
		now := time.Now().UTC()
		intent = domain.PricePinIntent{
			AttemptID: attemptID, DeploymentID: deploymentID, PriceVersionID: price.ID, PriceVersion: price.Version,
			SnapshotSHA256: digest, PricingSelectedAt: selectedAt, MetadataRevision: deployment.Revision,
			State: domain.PricePinPrepared, CreatedAt: now,
		}
		if err := intent.Validate(); err != nil {
			return err
		}
		encodedIntent, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		if err := pins.Put([]byte(attemptID), encodedIntent); err != nil {
			return err
		}
		if highWater.DeploymentID == "" {
			highWater = domain.DeploymentPricingHighWater{DeploymentID: deploymentID, LatestObservedPriceVersionID: price.ID, LatestSelectedAt: selectedAt, LatestObservedEffectiveFrom: price.EffectiveFrom, Revision: 1}
		} else {
			highWater.LatestSelectedAt = selectedAt
			if price.EffectiveFrom.After(highWater.LatestObservedEffectiveFrom) {
				highWater.LatestObservedEffectiveFrom = price.EffectiveFrom
				highWater.LatestObservedPriceVersionID = price.ID
			}
			highWater.Revision++
		}
		if err := highWater.Validate(); err != nil {
			return err
		}
		encodedHighWater, err := json.Marshal(highWater)
		if err != nil {
			return err
		}
		return highWaterBucket.Put([]byte(deploymentID), encodedHighWater)
	})
	if err != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, err
	}
	if quarantined {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, ErrPricingQuarantined
	}
	if outcome != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, outcome
	}
	state.mergeClockAnchor(pricingClockObservation{SelectedAt: selectedAt, ObservedAt: observedAt})
	return price, snapshot, intent, nil
}

func (s *Store) CommitDeploymentPricePin(ctx context.Context, attemptID, snapshotSHA256 string, ledgerSequence uint64, committedAt time.Time) (domain.PricePinIntent, error) {
	if err := ctx.Err(); err != nil {
		return domain.PricePinIntent{}, err
	}
	var intent domain.PricePinIntent
	// See PrepareDeploymentPricePin: expected outcomes travel in outcome so they
	// cannot abort a batch shared with unrelated deployments' commits.
	var outcome error
	err := s.batch(func(tx *bbolt.Tx) error {
		intent, outcome = domain.PricePinIntent{}, nil
		bucket := tx.Bucket(bucketDeploymentPricePins)
		raw := bucket.Get([]byte(attemptID))
		if raw == nil {
			outcome = ErrNotFound
			return nil
		}
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if intent.SnapshotSHA256 != snapshotSHA256 {
			outcome = ErrIdempotencyConflict
			return nil
		}
		if intent.State == domain.PricePinCommitted {
			if intent.LedgerSequence != ledgerSequence {
				outcome = ErrIdempotencyConflict
			}
			return nil
		}
		value := committedAt.UTC()
		intent.State, intent.LedgerSequence, intent.CommittedAt = domain.PricePinCommitted, ledgerSequence, &value
		if err := intent.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(attemptID), encoded)
	})
	if err != nil {
		return domain.PricePinIntent{}, err
	}
	if outcome != nil {
		return domain.PricePinIntent{}, outcome
	}
	return intent, nil
}

// DeletePreparedDeploymentPricePin stays on db.Update deliberately: it runs only
// on an attempt's local-failure path, so it has nothing to coalesce with and
// would only pay the batch delay.
func (s *Store) DeletePreparedDeploymentPricePin(ctx context.Context, attemptID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPricePins)
		raw := bucket.Get([]byte(attemptID))
		if raw == nil {
			return nil
		}
		var intent domain.PricePinIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if intent.State != domain.PricePinPrepared {
			return domain.ErrPriceVersionUnavailable
		}
		return bucket.Delete([]byte(attemptID))
	})
}

func quarantineIncoherentPricingHighWatersTx(tx *bbolt.Tx) error {
	highWaterBucket := tx.Bucket(bucketDeploymentPricingHighWater)
	updates := make(map[string]domain.DeploymentPricingHighWater)
	if err := highWaterBucket.ForEach(func(key, raw []byte) error {
		if raw == nil {
			return nil
		}
		var highWater domain.DeploymentPricingHighWater
		if err := json.Unmarshal(raw, &highWater); err != nil {
			return err
		}
		if err := highWater.Validate(); err != nil {
			return err
		}
		if highWater.Quarantined {
			return nil
		}
		var price domain.DeploymentPriceVersion
		priceRaw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(highWater.LatestObservedPriceVersionID))
		incoherent := priceRaw == nil
		if !incoherent {
			if err := json.Unmarshal(priceRaw, &price); err != nil {
				return err
			}
			incoherent = price.DeploymentID != highWater.DeploymentID ||
				!price.EffectiveFrom.Equal(highWater.LatestObservedEffectiveFrom)
		}
		if incoherent {
			highWater.Quarantined = true
			highWater.QuarantineReason = "restored_high_water_incoherent"
			highWater.Revision++
			updates[string(key)] = highWater
		}
		return nil
	}); err != nil {
		return err
	}
	for key, highWater := range updates {
		encoded, err := json.Marshal(highWater)
		if err != nil {
			return err
		}
		if err := highWaterBucket.Put([]byte(key), encoded); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecoverDeploymentPricePins(ctx context.Context, state *ledger.State) error {
	if state == nil {
		return errors.New("ledger state is required for price pin recovery")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := quarantineIncoherentPricingHighWatersTx(tx); err != nil {
			return err
		}
		bucket := tx.Bucket(bucketDeploymentPricePins)
		var deletes [][]byte
		var updates = make(map[string]domain.PricePinIntent)
		if err := bucket.ForEach(func(key, raw []byte) error {
			if raw == nil {
				return nil
			}
			var intent domain.PricePinIntent
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if err := intent.Validate(); err != nil {
				return err
			}
			lease, exists := state.AccountingLease(intent.AttemptID)
			if !exists {
				if intent.State == domain.PricePinPrepared {
					deletes = append(deletes, bytes.Clone(key))
					return nil
				}
				return fmt.Errorf("committed price pin %q has no Ledger lease", intent.AttemptID)
			}
			if lease.Event.PriceSnapshot == nil {
				return fmt.Errorf("price pin %q Ledger lease has no snapshot", intent.AttemptID)
			}
			digest, err := lease.Event.PriceSnapshot.Digest()
			if err != nil || digest != intent.SnapshotSHA256 {
				return fmt.Errorf("price pin %q snapshot digest mismatch: %w", intent.AttemptID, err)
			}
			if intent.State == domain.PricePinPrepared {
				now := time.Now().UTC()
				intent.State, intent.LedgerSequence, intent.CommittedAt = domain.PricePinCommitted, lease.Sequence, &now
				updates[string(key)] = intent
			} else if intent.LedgerSequence != lease.Sequence {
				return fmt.Errorf("price pin %q Ledger sequence mismatch", intent.AttemptID)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range deletes {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		for key, intent := range updates {
			if err := intent.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(intent)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(key), encoded); err != nil {
				return err
			}
		}
		return nil
	})
}

func selectDeploymentPriceVersionTx(tx *bbolt.Tx, deploymentID string, selectedAt time.Time) (domain.DeploymentPriceVersion, error) {
	timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deploymentID))
	if timeline == nil {
		return domain.DeploymentPriceVersion{}, domain.ErrPriceUnavailable
	}
	cursor := timeline.Cursor()
	key, priceID := cursor.Seek(deploymentPriceTimelineKey(selectedAt))
	if key == nil || bytes.Compare(key, deploymentPriceTimelineKey(selectedAt)) > 0 {
		key, priceID = cursor.Prev()
	}
	for key != nil {
		raw := tx.Bucket(bucketDeploymentPriceVersions).Get(priceID)
		if raw == nil {
			return domain.DeploymentPriceVersion{}, errors.New("deployment price timeline references a missing record")
		}
		var price domain.DeploymentPriceVersion
		if err := json.Unmarshal(raw, &price); err != nil {
			return domain.DeploymentPriceVersion{}, err
		}
		if price.CancelledAt == nil && !price.EffectiveFrom.After(selectedAt) {
			return price, price.Validate()
		}
		key, priceID = cursor.Prev()
	}
	return domain.DeploymentPriceVersion{}, domain.ErrPriceUnavailable
}

func (s *Store) CancelDeploymentPriceVersion(ctx context.Context, deploymentID, priceID, actor string, cancelledAt time.Time, expectedRevision uint64) (domain.DeploymentPriceVersion, error) {
	return s.cancelDeploymentPriceVersion(ctx, deploymentID, priceID, actor, cancelledAt, expectedRevision, nil)
}

func (s *Store) cancelDeploymentPriceVersion(ctx context.Context, deploymentID, priceID, actor string, cancelledAt time.Time, expectedRevision uint64, intent *domain.PricingAuditIntent) (domain.DeploymentPriceVersion, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	var price domain.DeploymentPriceVersion
	err := s.db.Update(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(priceID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &price); err != nil {
			return err
		}
		if price.DeploymentID != deploymentID {
			return ErrNotFound
		}
		if price.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if price.CancelledAt != nil || !cancelledAt.Before(price.EffectiveFrom) {
			return domain.ErrPriceVersionUnavailable
		}
		if err := tx.Bucket(bucketDeploymentPricePins).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var pin domain.PricePinIntent
			if err := json.Unmarshal(raw, &pin); err != nil {
				return err
			}
			if pin.DeploymentID == deploymentID && pin.PriceVersionID == priceID {
				return domain.ErrPriceVersionUnavailable
			}
			return nil
		}); err != nil {
			return err
		}
		price.CancelledAt = &cancelledAt
		price.CancelledBy = strings.TrimSpace(actor)
		price.Revision++
		if err := price.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(price)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPriceVersions).Put([]byte(price.ID), encoded); err != nil {
			return err
		}
		if intent != nil {
			return putPricingAuditIntentTx(tx, *intent)
		}
		return nil
	})
	return price, err
}

func putDeploymentPriceVersionTx(tx *bbolt.Tx, price domain.DeploymentPriceVersion) error {
	if err := price.Validate(); err != nil {
		return err
	}
	prices := tx.Bucket(bucketDeploymentPriceVersions)
	if prices.Get([]byte(price.ID)) != nil {
		return ErrAlreadyExists
	}
	timelineRoot := tx.Bucket(bucketDeploymentPriceTimeline)
	timeline, err := timelineRoot.CreateBucketIfNotExists([]byte(price.DeploymentID))
	if err != nil {
		return err
	}
	key := deploymentPriceTimelineKey(price.EffectiveFrom)
	if timeline.Get(key) != nil {
		return domain.ErrPriceTimelineConflict
	}
	encoded, err := json.Marshal(price)
	if err != nil {
		return err
	}
	if err := prices.Put([]byte(price.ID), encoded); err != nil {
		return err
	}
	return timeline.Put(key, []byte(price.ID))
}

func deploymentPriceTimelineKey(effectiveFrom time.Time) []byte {
	var key [12]byte
	seconds := effectiveFrom.UTC().Unix()
	binary.BigEndian.PutUint64(key[:8], uint64(seconds)^(uint64(1)<<63))
	binary.BigEndian.PutUint32(key[8:], uint32(effectiveFrom.UTC().Nanosecond()))
	return key[:]
}
