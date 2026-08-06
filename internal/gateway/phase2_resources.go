package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/contentscan"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/idempotency"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
)

type Phase2ResourceStore interface {
	PutProviderResource(context.Context, domain.ProviderResource, uint64) (domain.ProviderResource, error)
	ProviderResource(context.Context, string, string) (domain.ProviderResource, error)
	DeleteProviderResource(context.Context, string, string) error
	ProviderResourceByIdempotency(context.Context, string, domain.ProviderResourceKind, [32]byte) (domain.ProviderResource, error)
}

// Creation states for a resource whose upstream twin is created once and must
// never be created twice.
//
//	reserved  — the owner record exists; the provider has not been called yet
//	in_flight — the provider call has been made and its outcome is not known
//	unknown   — the call was made and failed in a way that leaves it ambiguous
//	completed — the upstream resource exists and its ID is recorded
//
// reserved and in_flight used to be the same state, which is what made a crash
// unrecoverable: a reservation left behind by a dead process is indistinguishable
// from a call that may have already reached the provider, so the only safe answer
// was to refuse the idempotency key until it expired seven to thirty days later.
// Splitting them makes the safe half recoverable and leaves the unsafe half
// exactly as closed as it was.
const (
	creationReserved  = "reserved"
	creationInFlight  = "in_flight"
	creationUnknown   = "unknown"
	creationCompleted = "completed"
)

type idempotencyVerdict int

const (
	// idempotencyFresh: no record for this key.
	idempotencyFresh idempotencyVerdict = iota
	// idempotencyCompleted: the resource exists; return it.
	idempotencyCompleted
	// idempotencyInProgress: another request holds it, or its outcome is unknown.
	idempotencyInProgress
	// idempotencyReclaim: a reservation from a process that is gone, made before
	// the provider was ever called. Safe to take over and retry.
	idempotencyReclaim
)

func (s *Service) classifyIdempotency(
	ctx context.Context,
	projectID string,
	kind domain.ProviderResourceKind,
	keyHash, fingerprint [32]byte,
) (domain.ProviderResource, idempotencyVerdict, error) {
	existing, err := s.resources.ProviderResourceByIdempotency(ctx, projectID, kind, keyHash)
	if err != nil {
		return domain.ProviderResource{}, idempotencyFresh, nil
	}
	if existing.RequestFingerprint != fingerprint {
		return existing, idempotencyInProgress,
			gatewayError("idempotency_conflict", "idempotency key was reused with a different request", 409, nil)
	}
	switch {
	case existing.CreationStatus == creationCompleted:
		return existing, idempotencyCompleted, nil
	case existing.CreationStatus == creationReserved && existing.ReservedBy != s.instanceID:
		// The data directory is held exclusively, so a reservation owned by any
		// other instance belongs to a process that is no longer running. It never
		// reached the provider, so nothing upstream can be duplicated by retrying.
		return existing, idempotencyReclaim, nil
	default:
		return existing, idempotencyInProgress, nil
	}
}

// markInFlight records that the provider is about to be called, so a crash from
// here on is remembered as ambiguous rather than as a reservation to reclaim.
func (s *Service) markInFlight(ctx context.Context, record domain.ProviderResource) (domain.ProviderResource, error) {
	record.CreationStatus = creationInFlight
	record.UpdatedAt = s.now()
	updated, err := s.resources.PutProviderResource(ctx, record, record.Revision)
	if err != nil {
		return record, gatewayError("resource_store_unavailable", "resource creation could not be recorded", 503, err)
	}
	return updated, nil
}

func (s *Service) updateResourceStatus(ctx context.Context, resource domain.ProviderResource, status string) error {
	if status == "" || status == resource.Status {
		return nil
	}
	resource.Status = status
	resource.UpdatedAt = s.now()
	if _, err := s.resources.PutProviderResource(ctx, resource, resource.Revision); err != nil {
		return gatewayError("resource_store_unavailable", "resource state could not be recorded", 503, err)
	}
	return nil
}

func (s *Service) writeResourceObject(idValue string, data []byte) (string, error) {
	if s.resourceObjectDir == "" {
		return "", errors.New("resource object directory is unavailable")
	}
	temporary, err := os.CreateTemp(s.resourceObjectDir, ".resource-*")
	if err != nil {
		return "", err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	name := idValue + ".object"
	if err := os.Rename(temporaryName, filepath.Join(s.resourceObjectDir, name)); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Service) resourceObjectPath(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || s.resourceObjectDir == "" {
		return "", errors.New("invalid resource object path")
	}
	return filepath.Join(s.resourceObjectDir, name), nil
}

func (s *Service) resourcePrincipal(ctx context.Context, key string) (auth.AuthResult, error) {
	principal, err := s.auth.Authenticate(key, s.now())
	if err != nil {
		return auth.AuthResult{}, gatewayError("invalid_api_key", "invalid API key", 401, err)
	}
	if err := authorizeSource(ctx, principal.Project); err != nil {
		return auth.AuthResult{}, err
	}
	if s.resources == nil {
		return auth.AuthResult{}, gatewayError("resource_store_unavailable", "resource storage is unavailable", 503, nil)
	}
	return principal, nil
}
func (s *Service) ownedTarget(resource domain.ProviderResource) (provider.Target, error) {
	for _, target := range s.registry.ResolveAll(resource.PublicModel) {
		if target.ProviderID == resource.ProviderID && target.DeploymentID == resource.DeploymentID &&
			target.ProfileID == resource.ProfileID && target.Region == resource.Region {
			return target, nil
		}
	}
	return provider.Target{}, gatewayError("resource_owner_unavailable", "the recorded resource owner is unavailable", 409, nil)
}

func (s *Service) CreateFile(ctx context.Context, key, route, idempotencyKey string, call provider.FileCreateCall) (provider.FileObject, error) {
	principal, err := s.resourcePrincipal(ctx, key)
	if err != nil {
		return provider.FileObject{}, err
	}
	if route == "" {
		return provider.FileObject{}, gatewayError("route_required", "Heimdall-Route is required for file creation", 400, nil)
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return provider.FileObject{}, gatewayError("invalid_idempotency_key", err.Error(), 400, err)
	}
	if err := s.contentScanner.ScanFile(call.Filename, call.ContentType, call.Data); err != nil {
		return provider.FileObject{}, gatewayError("content_rejected", "file was rejected by media policy", 400, err)
	}
	call.Filename, err = s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", call.Filename)
	if err != nil {
		return provider.FileObject{}, gatewayError("sensitive_data_detected", "file name contains secret material", 400, err)
	}
	if contentscan.Textual(call.ContentType, call.Data) {
		processed, e := s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", string(call.Data))
		if e != nil {
			return provider.FileObject{}, gatewayError("sensitive_data_detected", "file contains secret material", 400, e)
		}
		call.Data = []byte(processed)
	}
	if !slices.Contains(principal.Project.AllowedRoutes, route) {
		return provider.FileObject{}, gatewayError("model_not_allowed", "route is not allowed for this project", 403, nil)
	}
	targets := s.registry.ResolveCandidatesFor(route, provider.OperationFiles)
	if len(targets) != 1 {
		return provider.FileObject{}, gatewayError("ambiguous_resource_route", "file creation requires exactly one eligible deployment", 409, nil)
	}
	target := targets[0]
	adapter, ok := target.Adapter.(provider.ResourcePhase2Adapter)
	if !ok {
		return provider.FileObject{}, gatewayError("unsupported_feature", "file adapter is unavailable", 400, nil)
	}
	keyHash := sha256.Sum256([]byte(idempotencyKey))
	fingerprint := sha256.Sum256(append(append([]byte(route+"\x00"+call.Purpose+"\x00"+call.Filename+"\x00"), call.Data...), []byte(call.ContentType)...))
	existing, verdict, err := s.classifyIdempotency(ctx, principal.Project.ID, domain.ResourceFile, keyHash, fingerprint)
	if err != nil {
		return provider.FileObject{}, err
	}
	switch verdict {
	case idempotencyCompleted:
		return s.GetFile(ctx, key, existing.ID)
	case idempotencyInProgress:
		return provider.FileObject{}, gatewayError("idempotency_in_progress", "resource creation is already in progress or has unknown outcome", 409, nil)
	}
	externalID := existing.ID
	expectedRevision := existing.Revision
	if verdict != idempotencyReclaim {
		externalID, err = id.New("file")
		if err != nil {
			return provider.FileObject{}, gatewayError("internal_error", "unable to create resource ID", 500, err)
		}
		expectedRevision = 0
	}
	now := s.now()
	record := domain.ProviderResource{ID: externalID, Kind: domain.ResourceFile, ProjectID: principal.Project.ID, ProviderID: target.ProviderID, DeploymentID: target.DeploymentID, PublicModel: route, ProfileID: target.ProfileID, Region: target.Region, IdempotencyKeyHash: keyHash, RequestFingerprint: fingerprint, CreationStatus: creationReserved, ReservedBy: s.instanceID, Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour)}
	record, err = s.resources.PutProviderResource(ctx, record, expectedRevision)
	if err != nil {
		return provider.FileObject{}, gatewayError("idempotency_in_progress", "resource creation is already reserved", 409, err)
	}
	if record, err = s.markInFlight(ctx, record); err != nil {
		return provider.FileObject{}, err
	}
	requestID := ""
	call.RequestID = requestID
	var upstream provider.FileObject
	err = s.accountedPhase2(ctx, principal, route, target, int64(len(call.Data))/4+1, &requestID, func() error {
		call.RequestID = requestID
		var callErr error
		upstream, callErr = adapter.CreateFile(ctx, call)
		if callErr == nil {
			callErr = s.redactFileObject(principal.Project.RedactionPolicyID, &upstream)
		}
		return callErr
	})
	if err != nil {
		record.CreationStatus = creationUnknown
		record.UpdatedAt = s.now()
		_, _ = s.resources.PutProviderResource(ctx, record, record.Revision)
		return provider.FileObject{}, err
	}
	record.UpstreamID = upstream.ID
	objectPath, objectErr := s.writeResourceObject(record.ID, call.Data)
	if objectErr != nil {
		record.CreationStatus = creationUnknown
		record.UpdatedAt = s.now()
		_, _ = s.resources.PutProviderResource(ctx, record, record.Revision)
		return provider.FileObject{}, gatewayError("resource_store_unavailable", "file content could not be stored", 503, objectErr)
	}
	record.ObjectPath = objectPath
	record.ObjectContentType = call.ContentType
	record.CreationStatus = creationCompleted
	record.Status = upstream.Status
	if record.Status == "" {
		record.Status = "uploaded"
	}
	record.UpdatedAt = s.now()
	if _, err := s.resources.PutProviderResource(ctx, record, record.Revision); err != nil {
		return provider.FileObject{}, gatewayError("resource_store_unavailable", "file owner could not be recorded", 503, err)
	}
	upstream.ID = externalID
	return upstream, nil
}
func (s *Service) fileOwner(ctx context.Context, key, idValue string) (auth.AuthResult, domain.ProviderResource, provider.ResourcePhase2Adapter, error) {
	principal, err := s.resourcePrincipal(ctx, key)
	if err != nil {
		return principal, domain.ProviderResource{}, nil, err
	}
	resource, err := s.resources.ProviderResource(ctx, principal.Project.ID, idValue)
	if err != nil {
		return principal, resource, nil, gatewayError("resource_not_found", "resource was not found", 404, err)
	}
	if resource.Kind != domain.ResourceFile {
		return principal, resource, nil, gatewayError("resource_not_found", "resource was not found", 404, nil)
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return principal, resource, nil, err
	}
	adapter, ok := target.Adapter.(provider.ResourcePhase2Adapter)
	if !ok {
		return principal, resource, nil, gatewayError("resource_owner_unavailable", "file owner adapter is unavailable", 409, nil)
	}
	return principal, resource, adapter, nil
}
func (s *Service) GetFile(ctx context.Context, key, idValue string) (provider.FileObject, error) {
	principal, resource, adapter, err := s.fileOwner(ctx, key, idValue)
	if err != nil {
		return provider.FileObject{}, err
	}
	target, _ := s.ownedTarget(resource)
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var result provider.FileObject
	err = s.accountedPhase2(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var callErr error
		result, callErr = adapter.GetFile(ctx, requestID, resource.UpstreamID)
		if callErr == nil {
			callErr = s.redactFileObject(principal.Project.RedactionPolicyID, &result)
		}
		return callErr
	})
	if err != nil {
		return provider.FileObject{}, err
	}
	result.ID = resource.ID
	return result, nil
}
func (s *Service) DownloadFile(ctx context.Context, key, idValue string) (provider.FileContent, error) {
	principal, resource, _, err := s.fileOwner(ctx, key, idValue)
	if err != nil {
		return provider.FileContent{}, err
	}
	path, err := s.resourceObjectPath(resource.ObjectPath)
	if err != nil {
		return provider.FileContent{}, gatewayError("resource_store_unavailable", "file content is unavailable", 503, err)
	}
	target, _ := s.ownedTarget(resource)
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var data []byte
	err = s.accountedPhase2(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var readErr error
		data, readErr = os.ReadFile(path)
		return readErr
	})
	if err != nil {
		return provider.FileContent{}, gatewayError("resource_store_unavailable", "file content is unavailable", 503, err)
	}
	return provider.FileContent{Data: data, ContentType: resource.ObjectContentType}, nil
}

func (s *Service) redactFileObject(policyID string, result *provider.FileObject) error {
	var err error
	result.Filename, err = s.redactor.ProcessText(policyID, "outbound", result.Filename)
	if err != nil {
		return err
	}
	result.StatusDetails, err = s.redactor.ProcessText(policyID, "outbound", result.StatusDetails)
	return err
}
func (s *Service) DeleteFile(ctx context.Context, key, idValue string) (provider.FileDeleteResult, error) {
	principal, resource, adapter, err := s.fileOwner(ctx, key, idValue)
	if err != nil {
		return provider.FileDeleteResult{}, err
	}
	result := provider.FileDeleteResult{ID: resource.ID, Object: "file", Deleted: true}
	target, _ := s.ownedTarget(resource)
	target.FixedRequestMicrosUSD = 0
	freshDelete := resource.CleanupStatus == ""
	if freshDelete {
		resource.CleanupStatus = "deleting"
		resource.UpdatedAt = s.now()
		resource, err = s.resources.PutProviderResource(ctx, resource, resource.Revision)
		if err != nil {
			return provider.FileDeleteResult{}, gatewayError("resource_store_unavailable", "file delete intent could not be recorded", 503, err)
		}
	}
	if resource.CleanupStatus == "deleting" {
		shouldDelete := freshDelete
		if !freshDelete {
			requestID := ""
			var lookupErr error
			accountingErr := s.accountedPhase2(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
				_, lookupErr = adapter.GetFile(ctx, requestID, resource.UpstreamID)
				return lookupErr
			})
			switch {
			case lookupErr == nil:
				shouldDelete = true
			case providerHTTPNotFound(lookupErr):
				shouldDelete = false
			default:
				return provider.FileDeleteResult{}, accountingErr
			}
		}
		if shouldDelete {
			requestID := ""
			var deleteErr error
			accountingErr := s.accountedPhase2(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
				result, deleteErr = adapter.DeleteFile(ctx, requestID, resource.UpstreamID)
				return deleteErr
			})
			if deleteErr != nil && !providerHTTPNotFound(deleteErr) {
				return provider.FileDeleteResult{}, accountingErr
			}
		}
		resource.CleanupStatus = "pending"
		resource.UpdatedAt = s.now()
		resource, err = s.resources.PutProviderResource(ctx, resource, resource.Revision)
		if err != nil {
			return provider.FileDeleteResult{}, gatewayError("resource_store_unavailable", "file cleanup state could not be recorded", 503, err)
		}
	}
	if path, pathErr := s.resourceObjectPath(resource.ObjectPath); pathErr == nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return provider.FileDeleteResult{}, gatewayError("resource_store_unavailable", fmt.Sprintf("file object cleanup failed: %v", removeErr), 503, removeErr)
		}
	}
	if err := s.resources.DeleteProviderResource(ctx, principal.Project.ID, resource.ID); err != nil {
		return provider.FileDeleteResult{}, gatewayError("resource_store_unavailable", "file owner could not be deleted", 503, err)
	}
	result.ID = resource.ID
	return result, nil
}

func providerHTTPNotFound(err error) bool {
	var providerErr *provider.Error
	return errors.As(err, &providerErr) && providerErr.StatusCode == 404
}

// CleanupExpiredProviderResource performs trusted TTL maintenance while still
// honoring the immutable owner binding. File metadata is never discarded until
// the pinned upstream file and the private local object are both confirmed
// absent. Batch and async records are admitted here only after the store has
// classified them as terminal.
func (s *Service) CleanupExpiredProviderResource(ctx context.Context, resource domain.ProviderResource) error {
	if !resource.ExpiryReapable() || resource.ExpiresAt.After(s.now()) {
		return errors.New("provider resource is not eligible for expiry cleanup")
	}
	if resource.Kind != domain.ResourceFile {
		return s.resources.DeleteProviderResource(ctx, resource.ProjectID, resource.ID)
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return err
	}
	adapter, ok := target.Adapter.(provider.ResourcePhase2Adapter)
	if !ok {
		return gatewayError("resource_owner_unavailable", "file owner adapter is unavailable", 409, nil)
	}
	freshDelete := resource.CleanupStatus == ""
	if freshDelete {
		resource.CleanupStatus = "deleting"
		resource.UpdatedAt = s.now()
		resource, err = s.resources.PutProviderResource(ctx, resource, resource.Revision)
		if err != nil {
			return err
		}
	}
	if resource.CleanupStatus == "deleting" {
		shouldDelete := freshDelete
		requestID, requestErr := id.New("req")
		if requestErr != nil {
			return requestErr
		}
		if !freshDelete {
			_, lookupErr := adapter.GetFile(ctx, requestID, resource.UpstreamID)
			switch {
			case lookupErr == nil:
				shouldDelete = true
			case providerHTTPNotFound(lookupErr):
				shouldDelete = false
			default:
				return lookupErr
			}
		}
		if shouldDelete {
			_, deleteErr := adapter.DeleteFile(ctx, requestID, resource.UpstreamID)
			if deleteErr != nil && !providerHTTPNotFound(deleteErr) {
				return deleteErr
			}
		}
		resource.CleanupStatus = "pending"
		resource.UpdatedAt = s.now()
		resource, err = s.resources.PutProviderResource(ctx, resource, resource.Revision)
		if err != nil {
			return err
		}
	}
	if path, pathErr := s.resourceObjectPath(resource.ObjectPath); pathErr == nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	}
	return s.resources.DeleteProviderResource(ctx, resource.ProjectID, resource.ID)
}

func (s *Service) CreateBatch(ctx context.Context, key, idempotencyKey string, call provider.BatchCreateCall) (provider.BatchObject, error) {
	principal, file, adapter, err := s.fileOwner(ctx, key, call.InputFileID)
	if err != nil {
		return provider.BatchObject{}, err
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return provider.BatchObject{}, gatewayError("invalid_idempotency_key", err.Error(), 400, err)
	}
	for key, value := range call.Metadata {
		processed, e := s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", value)
		if e != nil {
			return provider.BatchObject{}, gatewayError("sensitive_data_detected", "batch metadata contains secret material", 400, e)
		}
		call.Metadata[key] = processed
	}
	call.InputFileID = file.UpstreamID
	encoded, _ := json.Marshal(call)
	keyHash := sha256.Sum256([]byte(idempotencyKey))
	fingerprint := sha256.Sum256(encoded)
	existing, verdict, err := s.classifyIdempotency(ctx, principal.Project.ID, domain.ResourceBatch, keyHash, fingerprint)
	if err != nil {
		return provider.BatchObject{}, err
	}
	switch verdict {
	case idempotencyCompleted:
		return s.GetBatch(ctx, key, existing.ID)
	case idempotencyInProgress:
		return provider.BatchObject{}, gatewayError("idempotency_in_progress", "batch creation is already in progress or unknown", 409, nil)
	}
	externalID := existing.ID
	expectedRevision := existing.Revision
	if verdict != idempotencyReclaim {
		externalID, err = id.New("batch")
		if err != nil {
			return provider.BatchObject{}, gatewayError("internal_error", "unable to create resource ID", 500, err)
		}
		expectedRevision = 0
	}
	now := s.now()
	record := domain.ProviderResource{ID: externalID, Kind: domain.ResourceBatch, ProjectID: principal.Project.ID, ProviderID: file.ProviderID, DeploymentID: file.DeploymentID, PublicModel: file.PublicModel, ProfileID: file.ProfileID, Region: file.Region, IdempotencyKeyHash: keyHash, RequestFingerprint: fingerprint, CreationStatus: creationReserved, ReservedBy: s.instanceID, Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour)}
	record, err = s.resources.PutProviderResource(ctx, record, expectedRevision)
	if err != nil {
		return provider.BatchObject{}, gatewayError("idempotency_in_progress", "batch creation is already reserved", 409, err)
	}
	if record, err = s.markInFlight(ctx, record); err != nil {
		return provider.BatchObject{}, err
	}
	requestID := ""
	call.RequestID = requestID
	var upstream provider.BatchObject
	target, _ := s.ownedTarget(file)
	err = s.accountedPhase2(ctx, principal, file.PublicModel, target, 1, &requestID, func() error {
		call.RequestID = requestID
		var callErr error
		upstream, callErr = adapter.CreateBatch(ctx, call)
		if callErr == nil {
			callErr = s.redactBatchObject(principal.Project.RedactionPolicyID, &upstream)
		}
		return callErr
	})
	if err != nil {
		record.CreationStatus = creationUnknown
		record.UpdatedAt = s.now()
		_, _ = s.resources.PutProviderResource(ctx, record, record.Revision)
		return provider.BatchObject{}, err
	}
	record.UpstreamID = upstream.ID
	record.CreationStatus = creationCompleted
	record.Status = upstream.Status
	record.UpdatedAt = s.now()
	if _, err := s.resources.PutProviderResource(ctx, record, record.Revision); err != nil {
		return provider.BatchObject{}, gatewayError("resource_store_unavailable", "batch owner could not be recorded", 503, err)
	}
	upstream.ID = externalID
	upstream.InputFileID = file.ID
	return upstream, nil
}
func (s *Service) batchOwner(ctx context.Context, key, idValue string) (auth.AuthResult, domain.ProviderResource, provider.ResourcePhase2Adapter, error) {
	principal, err := s.resourcePrincipal(ctx, key)
	if err != nil {
		return principal, domain.ProviderResource{}, nil, err
	}
	resource, err := s.resources.ProviderResource(ctx, principal.Project.ID, idValue)
	if err != nil || resource.Kind != domain.ResourceBatch {
		return principal, resource, nil, gatewayError("resource_not_found", "batch was not found", 404, err)
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return principal, resource, nil, err
	}
	adapter, ok := target.Adapter.(provider.ResourcePhase2Adapter)
	if !ok {
		return principal, resource, nil, gatewayError("resource_owner_unavailable", "batch owner adapter is unavailable", 409, nil)
	}
	return principal, resource, adapter, nil
}
func (s *Service) GetBatch(ctx context.Context, key, idValue string) (provider.BatchObject, error) {
	principal, resource, adapter, err := s.batchOwner(ctx, key, idValue)
	if err != nil {
		return provider.BatchObject{}, err
	}
	target, _ := s.ownedTarget(resource)
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var result provider.BatchObject
	err = s.accountedPhase2(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var callErr error
		result, callErr = adapter.GetBatch(ctx, requestID, resource.UpstreamID)
		if callErr == nil {
			callErr = s.redactBatchObject(principal.Project.RedactionPolicyID, &result)
		}
		return callErr
	})
	if err != nil {
		return provider.BatchObject{}, err
	}
	if err := s.updateResourceStatus(ctx, resource, result.Status); err != nil {
		return provider.BatchObject{}, err
	}
	result.ID = resource.ID
	return result, nil
}
func (s *Service) CancelBatch(ctx context.Context, key, idValue string) (provider.BatchObject, error) {
	principal, resource, adapter, err := s.batchOwner(ctx, key, idValue)
	if err != nil {
		return provider.BatchObject{}, err
	}
	target, _ := s.ownedTarget(resource)
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var result provider.BatchObject
	err = s.accountedPhase2(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var callErr error
		result, callErr = adapter.CancelBatch(ctx, requestID, resource.UpstreamID)
		if callErr == nil {
			callErr = s.redactBatchObject(principal.Project.RedactionPolicyID, &result)
		}
		return callErr
	})
	if err != nil {
		return provider.BatchObject{}, err
	}
	if err := s.updateResourceStatus(ctx, resource, result.Status); err != nil {
		return provider.BatchObject{}, err
	}
	result.ID = resource.ID
	return result, nil
}

func (s *Service) redactBatchObject(policyID string, result *provider.BatchObject) error {
	for key, value := range result.Metadata {
		processed, err := s.redactor.ProcessText(policyID, "outbound", value)
		if err != nil {
			return err
		}
		result.Metadata[key] = processed
	}
	processed, err := s.redactor.ProcessJSON(policyID, "outbound", result.RawErrors)
	if err != nil {
		return err
	}
	result.RawErrors = processed
	return nil
}

func (s *Service) StartAsyncInvoke(ctx context.Context, key, idempotencyKey string, request openaiapi.AsyncInvokeRequest) (provider.AsyncInvokeObject, error) {
	principal, err := s.resourcePrincipal(ctx, key)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return provider.AsyncInvokeObject{}, gatewayError("invalid_idempotency_key", err.Error(), 400, err)
	}
	request.Prompt, err = s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", request.Prompt)
	if err != nil {
		return provider.AsyncInvokeObject{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	if !slices.Contains(principal.Project.AllowedRoutes, request.Model) {
		return provider.AsyncInvokeObject{}, gatewayError("model_not_allowed", "model is not allowed for this project", 403, nil)
	}
	targets := s.registry.ResolveCandidatesFor(request.Model, provider.OperationAsyncInvoke)
	if len(targets) != 1 {
		return provider.AsyncInvokeObject{}, gatewayError("ambiguous_resource_route", "async invocation requires exactly one eligible deployment", 409, nil)
	}
	target := targets[0]
	adapter, ok := target.Adapter.(provider.BedrockPhase2Adapter)
	if !ok {
		return provider.AsyncInvokeObject{}, gatewayError("unsupported_feature", "async adapter is unavailable", 400, nil)
	}
	encoded, _ := json.Marshal(request)
	keyHash := sha256.Sum256([]byte(idempotencyKey))
	fingerprint := sha256.Sum256(encoded)
	existing, verdict, err := s.classifyIdempotency(ctx, principal.Project.ID, domain.ResourceAsyncInvoke, keyHash, fingerprint)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	switch verdict {
	case idempotencyCompleted:
		return s.GetAsyncInvoke(ctx, key, existing.ID)
	case idempotencyInProgress:
		return provider.AsyncInvokeObject{}, gatewayError("idempotency_in_progress", "async invocation is already in progress or unknown", 409, nil)
	}
	externalID := existing.ID
	expectedRevision := existing.Revision
	if verdict != idempotencyReclaim {
		externalID, err = id.New("async")
		if err != nil {
			return provider.AsyncInvokeObject{}, gatewayError("internal_error", "unable to create resource ID", 500, err)
		}
		expectedRevision = 0
	}
	now := s.now()
	record := domain.ProviderResource{ID: externalID, Kind: domain.ResourceAsyncInvoke, ProjectID: principal.Project.ID, ProviderID: target.ProviderID, DeploymentID: target.DeploymentID, PublicModel: request.Model, ProfileID: target.ProfileID, Region: target.Region, IdempotencyKeyHash: keyHash, RequestFingerprint: fingerprint, CreationStatus: creationReserved, ReservedBy: s.instanceID, Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour)}
	record, err = s.resources.PutProviderResource(ctx, record, expectedRevision)
	if err != nil {
		return provider.AsyncInvokeObject{}, gatewayError("idempotency_in_progress", "async invocation is already reserved", 409, err)
	}
	if record, err = s.markInFlight(ctx, record); err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	requestID := ""
	var upstream provider.AsyncInvokeObject
	err = s.accountedPhase2(ctx, principal, request.Model, target, int64(len(request.Prompt))/4+1, &requestID, func() error {
		var callErr error
		upstream, callErr = adapter.StartAsyncInvoke(ctx, provider.AsyncInvokeCall{RequestID: requestID, ProviderModel: target.ProviderModel, Prompt: request.Prompt, S3OutputURI: request.S3OutputURI, DurationSeconds: request.DurationSeconds, Dimension: request.Dimension, FPS: request.FPS, Seed: request.Seed})
		return callErr
	})
	if err != nil {
		record.CreationStatus = creationUnknown
		record.UpdatedAt = s.now()
		_, _ = s.resources.PutProviderResource(ctx, record, record.Revision)
		return provider.AsyncInvokeObject{}, err
	}
	record.UpstreamID = upstream.InvocationARN
	record.CreationStatus = creationCompleted
	record.Status = upstream.Status
	record.UpdatedAt = s.now()
	if _, err := s.resources.PutProviderResource(ctx, record, record.Revision); err != nil {
		return provider.AsyncInvokeObject{}, gatewayError("resource_store_unavailable", "async owner could not be recorded", 503, err)
	}
	upstream.InvocationARN = externalID
	return upstream, nil
}
func (s *Service) GetAsyncInvoke(ctx context.Context, key, idValue string) (provider.AsyncInvokeObject, error) {
	principal, err := s.resourcePrincipal(ctx, key)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	resource, err := s.resources.ProviderResource(ctx, principal.Project.ID, idValue)
	if err != nil || resource.Kind != domain.ResourceAsyncInvoke {
		return provider.AsyncInvokeObject{}, gatewayError("resource_not_found", "async invocation was not found", 404, err)
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	adapter, ok := target.Adapter.(provider.BedrockPhase2Adapter)
	if !ok {
		return provider.AsyncInvokeObject{}, gatewayError("resource_owner_unavailable", "async owner adapter is unavailable", 409, nil)
	}
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var result provider.AsyncInvokeObject
	err = s.accountedPhase2(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var callErr error
		result, callErr = adapter.GetAsyncInvoke(ctx, requestID, resource.UpstreamID)
		return callErr
	})
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	if err := s.updateResourceStatus(ctx, resource, result.Status); err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	result.InvocationARN = resource.ID
	return result, nil
}
func (s *Service) CancelAsyncInvoke(ctx context.Context, key, idValue string) (provider.AsyncInvokeObject, error) {
	principal, err := s.resourcePrincipal(ctx, key)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	resource, err := s.resources.ProviderResource(ctx, principal.Project.ID, idValue)
	if err != nil || resource.Kind != domain.ResourceAsyncInvoke {
		return provider.AsyncInvokeObject{}, gatewayError("resource_not_found", "async invocation was not found", 404, err)
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	if _, ok := target.Adapter.(provider.BedrockPhase2Adapter); !ok {
		return provider.AsyncInvokeObject{}, gatewayError("resource_owner_unavailable", "async owner adapter is unavailable", 409, nil)
	}
	return provider.AsyncInvokeObject{}, gatewayError("provider_cancel_unsupported", "Amazon Bedrock Runtime does not provide cancellation for accepted async invocations", 409, nil)
}
