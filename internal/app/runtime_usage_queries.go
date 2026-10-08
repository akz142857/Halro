package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/usage"
)

// Keep the selected Usage query view and HTTP response lifetime in the Usage
// subsystem. The legacy Aggregate is explicit; a disk read never falls back to
// it. Startup/maintenance still owns constructing and retiring the physical DB.
// Standalone selects a fresh disk owner. Member roles retain the explicit
// legacy view until their role-permitted disk migration is implemented.
type runtimeUsage struct {
	*usage.Aggregate
	disk    *usage.DiskProjection
	owner   *runtimeUsageOwner
	reads   usageSummaryLifecycle
	metrics usageMetricsLifecycle
}

var errUsageQueryInvalid = errors.New("invalid usage page limit, account region or provider failure reason")

func validateUsageQuery(limit int, region domain.ProviderRegionID, reason provider.FailureReason) error {
	if limit < 1 || limit > 100 || region != domain.RegionNone && region != domain.RegionCN && region != domain.RegionGlobal || !reason.Valid() {
		return errUsageQueryInvalid
	}
	return nil
}

func (u *runtimeUsage) queryAttempts(ctx context.Context, q usage.AttemptQuery, cursor string) (usage.AttemptPage, error) {
	if err := validateUsageQuery(q.Limit, q.AccountRegionID, q.ProviderFailureReason); err != nil {
		return usage.AttemptPage{}, err
	}
	if u.disk != nil {
		return u.disk.QueryAttemptsHTTP(ctx, q, cursor, usageHTTPPageLimits())
	}
	return u.Aggregate.QueryAttempts(q)
}

func (u *runtimeUsage) queryFailures(ctx context.Context, q usage.FailureQuery, cursor string) (usage.FailurePage, error) {
	if err := validateUsageQuery(q.Limit, q.AccountRegionID, q.ProviderFailureReason); err != nil {
		return usage.FailurePage{}, err
	}
	if u.disk != nil {
		return u.disk.QueryFailedRequestsHTTP(ctx, q, cursor, usageHTTPPageLimits())
	}
	return u.Aggregate.QueryFailedRequests(q)
}

func (u *runtimeUsage) requestDetail(ctx context.Context, id string) (usage.RequestDetail, bool, error) {
	if u.disk != nil {
		return u.disk.ReadRequestDetailHTTP(ctx, id, usage.DiskDetailLimits{Pages: 128, Attempts: 4096, InputBytes: 64 << 20, OutputBytes: 8 << 20, MaxDuration: time.Minute, Page: usageHTTPPageLimits()})
	}
	detail, found := u.Aggregate.RequestDetail(id)
	return detail, found, ctx.Err()
}

func usageHTTPPageLimits() usage.DiskUsagePageLimits {
	return usage.DiskUsagePageLimits{ScanRows: 160, InputBytes: 8 << 20, OutputBytes: 8 << 20}
}

func (u *runtimeUsage) decodeCursor(raw string) (uint64, error) {
	if u.disk != nil {
		// Verification happens with the selected owner and complete query.
		return 0, nil
	}
	return usage.DecodeCursor(raw)
}

func (r *Runtime) beginUsageRead(writer http.ResponseWriter, request *http.Request) (*http.Request, func(), bool) {
	ctx, cancel := context.WithTimeout(request.Context(), time.Minute)
	if r.usage == nil {
		cancel()
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage unavailable"})
		return request, nil, false
	}
	release, err := r.usage.reads.enter(ctx)
	if err != nil {
		cancel()
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage unavailable"})
		return request, nil, false
	}
	finish := func() { release(); cancel() }
	if len(request.URL.RawQuery) > 1<<20 {
		finish()
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "usage query exceeds limits"})
		return request, nil, false
	}
	return request.WithContext(ctx), finish, true
}

func (r *Runtime) usageReadError(writer http.ResponseWriter, err error) {
	if r.usage.disk == nil || errors.Is(err, usage.ErrDiskUsageCursor) || errors.Is(err, errUsageQueryInvalid) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage unavailable"})
}

func (r *Runtime) writeUsageRead(writer http.ResponseWriter, request *http.Request, value any) {
	raw, err := json.Marshal(value)
	if err == nil && len(raw)+1 > 8<<20 {
		err = usage.ErrDiskUsageLimit
	}
	if err == nil {
		err = r.usage.reads.check(request.Context())
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "usage unavailable"})
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(append(raw, '\n'))
}

func (r *Runtime) withdrawUsageReads() {
	if r.usage != nil {
		r.usage.reads.withdraw()
	}
}

func (r *Runtime) drainUsageReads(ctx context.Context) error {
	if r.usage == nil {
		return ctx.Err()
	}
	return r.usage.reads.drain(ctx)
}
