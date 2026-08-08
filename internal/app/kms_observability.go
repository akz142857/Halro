package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/id"
	corekms "github.com/akz142857/Halro/internal/kms"
	"github.com/akz142857/Halro/internal/masterkey"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

type kmsCallMetricKey struct {
	Operation  corekms.Operation
	Status     string
	ErrorClass string
}

type kmsUnlockMetricKey struct {
	Purpose    masterkey.KeySlotPurpose
	Status     string
	ErrorClass string
}

type kmsProcessMetrics struct {
	mu            sync.Mutex
	calls         map[kmsCallMetricKey]uint64
	durationNanos map[kmsCallMetricKey]uint64
	unlocks       map[kmsUnlockMetricKey]uint64
}

type kmsMetricsSnapshot struct {
	Calls         map[kmsCallMetricKey]uint64
	DurationNanos map[kmsCallMetricKey]uint64
	Unlocks       map[kmsUnlockMetricKey]uint64
}

var processKMSMetrics = kmsProcessMetrics{
	calls:         make(map[kmsCallMetricKey]uint64),
	durationNanos: make(map[kmsCallMetricKey]uint64),
	unlocks:       make(map[kmsUnlockMetricKey]uint64),
}

func observeKMSCall(operation corekms.Operation, started time.Time, err error) {
	key := kmsCallMetricKey{Operation: operation, Status: "success", ErrorClass: "none"}
	if err != nil {
		key.Status = "error"
		key.ErrorClass = string(corekms.Classify(err))
	}
	processKMSMetrics.mu.Lock()
	processKMSMetrics.calls[key]++
	processKMSMetrics.durationNanos[key] += uint64(time.Since(started))
	processKMSMetrics.mu.Unlock()
}

func observeKMSUnlock(purpose masterkey.KeySlotPurpose, err error) {
	key := kmsUnlockMetricKey{Purpose: purpose, Status: "success", ErrorClass: "none"}
	if err != nil {
		key.Status = "error"
		key.ErrorClass = string(corekms.Classify(err))
	}
	processKMSMetrics.mu.Lock()
	processKMSMetrics.unlocks[key]++
	processKMSMetrics.mu.Unlock()
}

func snapshotKMSMetrics() kmsMetricsSnapshot {
	processKMSMetrics.mu.Lock()
	defer processKMSMetrics.mu.Unlock()
	snapshot := kmsMetricsSnapshot{
		Calls:         make(map[kmsCallMetricKey]uint64, len(processKMSMetrics.calls)),
		DurationNanos: make(map[kmsCallMetricKey]uint64, len(processKMSMetrics.durationNanos)),
		Unlocks:       make(map[kmsUnlockMetricKey]uint64, len(processKMSMetrics.unlocks)),
	}
	for key, value := range processKMSMetrics.calls {
		snapshot.Calls[key] = value
	}
	for key, value := range processKMSMetrics.durationNanos {
		snapshot.DurationNanos[key] = value
	}
	for key, value := range processKMSMetrics.unlocks {
		snapshot.Unlocks[key] = value
	}
	return snapshot
}

type observedKMSWrapper struct{ wrapped corekms.Wrapper }

func (w observedKMSWrapper) Provider() string { return w.wrapped.Provider() }

func (w observedKMSWrapper) Wrap(ctx context.Context, request corekms.WrapRequest) (corekms.WrapResult, error) {
	started := time.Now()
	result, err := w.wrapped.Wrap(ctx, request)
	observeKMSCall(corekms.OperationWrap, started, err)
	recordKMSProviderAudit(ctx, corekms.OperationWrap, result.ProviderRequestID, err)
	return result, err
}

func (w observedKMSWrapper) Unwrap(ctx context.Context, request corekms.UnwrapRequest) (corekms.UnwrapResult, error) {
	started := time.Now()
	result, err := w.wrapped.Unwrap(ctx, request)
	observeKMSCall(corekms.OperationUnwrap, started, err)
	recordKMSProviderAudit(ctx, corekms.OperationUnwrap, result.ProviderRequestID, err)
	return result, err
}

func observeKMSWrapper(wrapper corekms.Wrapper) corekms.Wrapper {
	if _, observed := wrapper.(observedKMSWrapper); observed {
		return wrapper
	}
	return observedKMSWrapper{wrapped: wrapper}
}

type kmsProviderAudit struct {
	OccurredAt        time.Time
	Operation         corekms.Operation
	Outcome           string
	ErrorClass        string
	ProviderRequestID string
}

type kmsAuditRecorder struct {
	mu     sync.Mutex
	events []kmsProviderAudit
}

type kmsAuditContextKey struct{}

func withKMSAuditRecorder(ctx context.Context, recorder *kmsAuditRecorder) context.Context {
	return context.WithValue(ctx, kmsAuditContextKey{}, recorder)
}

func kmsAuditRecorderFromContext(ctx context.Context) *kmsAuditRecorder {
	recorder, _ := ctx.Value(kmsAuditContextKey{}).(*kmsAuditRecorder)
	return recorder
}

func recordKMSProviderAudit(ctx context.Context, operation corekms.Operation, requestID string, err error) {
	recorder, _ := ctx.Value(kmsAuditContextKey{}).(*kmsAuditRecorder)
	if recorder == nil {
		return
	}
	event := kmsProviderAudit{
		OccurredAt: time.Now().UTC(), Operation: operation, Outcome: "success",
		ErrorClass: "none", ProviderRequestID: requestID,
	}
	if err != nil {
		event.Outcome = "error"
		event.ErrorClass = string(corekms.Classify(err))
		var classified *corekms.Error
		if errors.As(err, &classified) {
			event.ProviderRequestID = classified.ProviderRequestID
		}
	}
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func (r *kmsAuditRecorder) snapshot() []kmsProviderAudit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]kmsProviderAudit(nil), r.events...)
}

func appendKMSProviderAudit(ctx context.Context, log *audit.Log, store *boltstore.Store, recorder *kmsAuditRecorder) error {
	return appendKMSProviderAuditAs(ctx, log, store, recorder, "system")
}

func appendKMSProviderAuditAs(ctx context.Context, log *audit.Log, store *boltstore.Store, recorder *kmsAuditRecorder, actorType string) error {
	for _, observed := range recorder.snapshot() {
		eventID, err := id.New("aud")
		if err != nil {
			return err
		}
		if _, err := log.Append(ctx, audit.Event{
			EventID: eventID, OccurredAt: observed.OccurredAt, ActorType: actorType,
			Action: "security.kms.call", TargetType: "kms_operation", TargetID: string(observed.Operation),
			Outcome: observed.Outcome, ReasonCode: observed.ErrorClass, CorrelationID: observed.ProviderRequestID,
		}); err != nil {
			return err
		}
	}
	return checkpointAudit(store, log.Summary())
}

// appendOfflineKMSProviderAudit persists the provider calls observed by an
// offline CLI operation once that operation has unlocked the Audit HMAC key.
// Calls that cannot unlock any trusted Slot remain available in CloudTrail but
// cannot be authenticated into the local Audit chain.
func appendOfflineKMSProviderAudit(ctx context.Context, cfg config.Config, auditKey []byte, recorder *kmsAuditRecorder) error {
	if recorder == nil || len(recorder.snapshot()) == 0 {
		return nil
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer store.Close()
	return appendOfflineKMSProviderAuditToStore(ctx, cfg, auditKey, recorder, store)
}

func appendOfflineKMSProviderAuditToStore(ctx context.Context, cfg config.Config, auditKey []byte, recorder *kmsAuditRecorder, store *boltstore.Store) error {
	if recorder == nil || len(recorder.snapshot()) == 0 {
		return nil
	}
	log, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		return err
	}
	defer log.Close()
	if err := reconcileAuditCheckpoint(store, log.Summary()); err != nil {
		return err
	}
	return appendKMSProviderAuditAs(ctx, log, store, recorder, "local_cli")
}

func finishOfflineKMSProviderAudit(cfg config.Config, auditKey []byte, recorder *kmsAuditRecorder, operationErr error) error {
	auditErr := appendOfflineKMSProviderAudit(context.Background(), cfg, auditKey, recorder)
	return joinOfflineKMSProviderAuditError(operationErr, auditErr)
}

func finishOfflineKMSProviderAuditToStore(cfg config.Config, auditKey []byte, recorder *kmsAuditRecorder, store *boltstore.Store, operationErr error) error {
	auditErr := appendOfflineKMSProviderAuditToStore(context.Background(), cfg, auditKey, recorder, store)
	return joinOfflineKMSProviderAuditError(operationErr, auditErr)
}

func joinOfflineKMSProviderAuditError(operationErr, auditErr error) error {
	if auditErr == nil {
		return operationErr
	}
	auditErr = errors.Join(errors.New("persist offline KMS provider Audit"), auditErr)
	if operationErr != nil {
		return errors.Join(operationErr, auditErr)
	}
	return auditErr
}

func lastKMSRecoveryUse(log *audit.Log, recoverySlotID string) (time.Time, error) {
	var latest time.Time
	_, err := log.Replay(func(record audit.Record) error {
		reasonAllowed := record.Event.ReasonCode == "break_glass_recovery" || record.Event.ReasonCode == "break_glass_restore"
		if recoverySlotID != "" && record.Event.Action == "security.master_key.recovery_used" && record.Event.Outcome == "success" &&
			record.Event.TargetType == "master_key_slot" && record.Event.TargetID == recoverySlotID && reasonAllowed && record.Event.OccurredAt.After(latest) {
			latest = record.Event.OccurredAt
		}
		return nil
	})
	return latest, err
}
