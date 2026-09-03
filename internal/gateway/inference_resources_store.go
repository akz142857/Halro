package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/contentscan"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/idempotency"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

type InferenceResourcesResourceStore interface {
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

// ResourceObjectSealer seals and opens the bytes Halro keeps for a provider
// resource. The gateway names the interface rather than importing the vault so
// that the object directory stays the gateway's business and the key material
// stays the vault's.
type ResourceObjectSealer interface {
	EncryptResourceObject(resourceID, projectID string, plaintext []byte) ([]byte, error)
	DecryptResourceObject(resourceID, projectID string, envelope []byte) ([]byte, error)
}

// writeResourceObject seals the bytes and writes the envelope. Both callers
// hand it the record that will carry the resulting path, which is what the seal
// is bound to: an object is readable only through the record that names it, and
// only inside the install whose master key derived the scope.
func (s *Service) writeResourceObject(resourceID, projectID string, data []byte) (string, error) {
	if s.resourceObjectDir == "" || s.resourceObjectSealer == nil {
		return "", errors.New("resource object directory is unavailable")
	}
	sealed, err := s.resourceObjectSealer.EncryptResourceObject(resourceID, projectID, data)
	if err != nil {
		return "", err
	}
	data = sealed
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
	name := resourceID + ".object"
	if err := os.Rename(temporaryName, filepath.Join(s.resourceObjectDir, name)); err != nil {
		return "", err
	}
	if err := durable.SyncDirectory(s.resourceObjectDir); err != nil {
		return "", err
	}
	return name, nil
}

// readResourceObject opens what writeResourceObject sealed. A record whose
// object predates sealing reads as a distinct error rather than as corruption,
// because the two call for different answers: the first is reclaimed at startup,
// the second is a key or integrity problem nobody should paper over.
func (s *Service) readResourceObject(resource domain.ProviderResource) ([]byte, error) {
	path, err := s.resourceObjectPath(resource.ObjectPath)
	if err != nil {
		return nil, err
	}
	sealed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if s.resourceObjectSealer == nil {
		return nil, errors.New("resource object sealer is unavailable")
	}
	return s.resourceObjectSealer.DecryptResourceObject(resource.ID, resource.ProjectID, sealed)
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
	// The resource paths redact file names, textual uploads and batch results,
	// and the redaction engine answers a policy lookup miss with "no policy" —
	// the fail-open direction. resolveRequest already refuses a request whose
	// Project names a policy the live snapshot does not hold; this is the same
	// belt for the plane that does not pass through resolveRequest.
	if err := s.assertPolicySnapshotsCoverProject(principal); err != nil {
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
		return provider.FileObject{}, gatewayError("route_required", "Halro-Route is required for file creation", 400, nil)
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
	if !slices.Contains(principal.Project.AllowedModels, route) {
		return provider.FileObject{}, gatewayError("model_not_allowed", "route is not allowed for this project", 403, nil)
	}
	targets := s.registry.ResolveCandidatesFor(route, provider.OperationFiles)
	if len(targets) != 1 {
		return provider.FileObject{}, gatewayError("ambiguous_resource_route", "file creation requires exactly one eligible deployment", 409, nil)
	}
	target := targets[0]
	// Whether this upload has a southbound call is the profile's declaration,
	// not something inferred from the adapter's shape. PrimitiveHalroLocalFiles
	// means Halro keeps the bytes and the upstream is never told; every other
	// primitive means there is an upload to make and an adapter that must be
	// able to make it.
	localOnly := false
	if resolved, ok := target.ResolveOperation(provider.OperationFiles); ok {
		localOnly = resolved.ProviderPrimitive() == provider.PrimitiveHalroLocalFiles
	}
	var adapter provider.ResourceFilesAdapter
	if !localOnly {
		resourceAdapter, ok := target.Adapter.(provider.ResourceFilesAdapter)
		if !ok {
			return provider.FileObject{}, gatewayError("unsupported_feature", "file adapter is unavailable", 400, nil)
		}
		adapter = resourceAdapter
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
	// in-flight means "the call may already have reached the upstream", which is
	// what makes an interrupted attempt unreplayable and holds the idempotency
	// key until the record expires. A local upload has no upstream to have
	// reached: an interrupted one created nothing, so it stays reserved and the
	// key stays reclaimable.
	if !localOnly {
		if record, err = s.markInFlight(ctx, record); err != nil {
			return provider.FileObject{}, err
		}
	}
	requestID := ""
	call.RequestID = requestID
	var upstream provider.FileObject
	// A local upload is still metered — the envelope carries Token Guard, the
	// limiters and the request record — but at one unit and no fixed price.
	// Charging the upstream's per-byte rate for bytes that never left the host
	// would bill a call that was never made.
	units := int64(len(call.Data))/4 + 1
	if localOnly {
		units = 1
		target.FixedRequestMicrosUSD = 0
	}
	err = s.accountedInferenceResources(ctx, principal, route, target, units, &requestID, func() error {
		call.RequestID = requestID
		if localOnly {
			// created_at comes from the record, the same instant the later GET
			// reports. It used to be left at zero here, so a file's creation
			// response said it was created at the epoch while every subsequent read
			// of the same file gave the real time — and clients sort by this field.
			upstream = provider.FileObject{Object: "file", Bytes: int64(len(call.Data)), CreatedAt: record.CreatedAt.Unix(), Filename: call.Filename, Purpose: call.Purpose, Status: "uploaded"}
			return s.redactFileObject(principal.Project.RedactionPolicyID, &upstream)
		}
		var callErr error
		upstream, callErr = adapter.CreateFile(ctx, call)
		if callErr == nil {
			callErr = s.redactFileObject(principal.Project.RedactionPolicyID, &upstream)
		}
		return callErr
	})
	if err != nil {
		// Unknown is for an outcome nobody can determine. A local failure is
		// determinate — nothing was created — so the reservation is left as it
		// is rather than being reported as ambiguous and freezing the key.
		if !localOnly {
			record.CreationStatus = creationUnknown
			record.UpdatedAt = s.now()
			_, _ = s.resources.PutProviderResource(ctx, record, record.Revision)
		}
		return provider.FileObject{}, err
	}
	record.UpstreamID = upstream.ID
	objectPath, objectErr := s.writeResourceObject(record.ID, record.ProjectID, call.Data)
	if objectErr != nil {
		record.CreationStatus = creationUnknown
		record.UpdatedAt = s.now()
		_, _ = s.resources.PutProviderResource(ctx, record, record.Revision)
		return provider.FileObject{}, gatewayError("resource_store_unavailable", "file content could not be stored", 503, objectErr)
	}
	record.ObjectPath = objectPath
	record.ObjectBytes = int64(len(call.Data))
	record.ObjectContentType = call.ContentType
	record.ObjectFilename = call.Filename
	record.ObjectPurpose = call.Purpose
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
func (s *Service) fileOwner(ctx context.Context, key, idValue string) (auth.AuthResult, domain.ProviderResource, provider.ResourceFilesAdapter, error) {
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
	// A file with no upstream twin needs an owner, not an adapter. Demanding one
	// here would refuse the local paths before they are reached — every caller
	// below branches on the empty UpstreamID, and none of them would get the
	// chance. A nil adapter is safe precisely because those callers never use it.
	if resource.UpstreamID == "" {
		return principal, resource, nil, nil
	}
	adapter, ok := target.Adapter.(provider.ResourceFilesAdapter)
	if !ok {
		return principal, resource, nil, gatewayError("resource_owner_unavailable", "file owner adapter is unavailable", 409, nil)
	}
	return principal, resource, adapter, nil
}

// localFileObject describes a file Halro holds and the upstream does not. The
// record is the whole truth about it, so nothing is fetched.
//
// It still runs inside the accounting envelope at zero cost, the way every other
// local read does. The envelope is not only about money: it is where Token
// Guard, the rate limiters, the concurrency leases and the request record live.
// Answering outside it would make this the one file operation a project could
// poll without limit and without leaving a trace, which is a strange privilege
// for the operation that happens to need no provider call.
func (s *Service) localFileObject(ctx context.Context, principal auth.AuthResult, resource domain.ProviderResource) (provider.FileObject, error) {
	result := provider.FileObject{
		ID: resource.ID, Object: "file", Bytes: resource.ObjectBytes,
		CreatedAt: resource.CreatedAt.Unix(), Filename: resource.ObjectFilename,
		Purpose: resource.ObjectPurpose, Status: resource.Status,
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return provider.FileObject{}, err
	}
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	if err := s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		return s.redactFileObject(principal.Project.RedactionPolicyID, &result)
	}); err != nil {
		return provider.FileObject{}, err
	}
	return result, nil
}

// forgetLocalResource removes what Halro holds for a resource the upstream never
// had. The object goes first: a record without its object is recoverable, an
// object without its record is a file nothing can name or reap.
func (s *Service) forgetLocalResource(ctx context.Context, resource domain.ProviderResource) error {
	if path, pathErr := s.resourceObjectPath(resource.ObjectPath); pathErr == nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	}
	return s.resources.DeleteProviderResource(ctx, resource.ProjectID, resource.ID)
}

// batchInputFile resolves a batch's input file for ownership alone. It is
// fileOwner without the adapter requirement, because a batch whose provider
// takes its requests inline needs the bytes rather than an upstream handle.
func (s *Service) batchInputFile(ctx context.Context, key, idValue string) (auth.AuthResult, domain.ProviderResource, error) {
	principal, err := s.resourcePrincipal(ctx, key)
	if err != nil {
		return principal, domain.ProviderResource{}, err
	}
	resource, err := s.resources.ProviderResource(ctx, principal.Project.ID, idValue)
	if err != nil || resource.Kind != domain.ResourceFile {
		return principal, resource, gatewayError("resource_not_found", "resource was not found", 404, err)
	}
	return principal, resource, nil
}

func (s *Service) GetFile(ctx context.Context, key, idValue string) (provider.FileObject, error) {
	principal, resource, adapter, err := s.fileOwner(ctx, key, idValue)
	if err != nil {
		return provider.FileObject{}, err
	}
	// A file with no upstream twin is answered from the record. There is nothing
	// to ask: Halro holds these bytes and the upstream was never told they
	// exist. See ADR 0021 — an empty UpstreamID is an ordinary state, not a
	// resource in a broken one.
	if resource.UpstreamID == "" {
		return s.localFileObject(ctx, principal, resource)
	}
	target, _ := s.ownedTarget(resource)
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var result provider.FileObject
	err = s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
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
	// A file produced by a batch has no local object: Halro never uploaded those
	// bytes. Serving it means asking the upstream, which is bounded by the
	// adapter's own response ceiling — the same ceiling every other provider
	// response is read under. Nothing here decides to store large results; it
	// declines to pretend the file is missing when the upstream still has it.
	if resource.ObjectPath == "" {
		return s.downloadUpstreamFile(ctx, principal, resource)
	}
	target, _ := s.ownedTarget(resource)
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var data []byte
	err = s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var readErr error
		data, readErr = s.readResourceObject(resource)
		return readErr
	})
	if err != nil {
		return provider.FileContent{}, gatewayError("resource_store_unavailable", "file content is unavailable", 503, err)
	}
	return provider.FileContent{Data: data, ContentType: resource.ObjectContentType}, nil
}

func (s *Service) downloadUpstreamFile(ctx context.Context, principal auth.AuthResult, resource domain.ProviderResource) (provider.FileContent, error) {
	target, err := s.ownedTarget(resource)
	if err != nil {
		return provider.FileContent{}, err
	}
	adapter, ok := target.Adapter.(provider.ResourceFilesAdapter)
	if !ok {
		return provider.FileContent{}, gatewayError("resource_owner_unavailable", "file owner adapter is unavailable", 409, nil)
	}
	if resource.UpstreamID == "" {
		return provider.FileContent{}, gatewayError("resource_store_unavailable", "file content is unavailable", 503, nil)
	}
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var content provider.FileContent
	err = s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var callErr error
		content, callErr = adapter.DownloadFile(ctx, requestID, resource.UpstreamID)
		return callErr
	})
	if err != nil {
		return provider.FileContent{}, err
	}
	return content, nil
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
	// Interactive delete needs the same branch expiry cleanup already has. Slice
	// 1 gave it to the reaper and not to this path, which would have called the
	// upstream with an empty identifier — a request nobody could answer, about a
	// file it never had.
	if resource.UpstreamID == "" {
		if err := s.forgetLocalResource(ctx, resource); err != nil {
			return provider.FileDeleteResult{}, gatewayError("resource_store_unavailable", "file could not be deleted", 503, err)
		}
		return result, nil
	}
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
			accountingErr := s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
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
			accountingErr := s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
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
	// A file with no upstream twin skips the upstream half of the dance and goes
	// straight to removing what Halro actually holds. Running the delete-confirm
	// ladder against an upstream that never had the file would answer 404 and
	// be read as "already gone", which is the right conclusion reached by
	// asking a question that should not have been asked.
	if resource.UpstreamID == "" {
		return s.forgetLocalResource(ctx, resource)
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return err
	}
	adapter, ok := target.Adapter.(provider.ResourceFilesAdapter)
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
	// The input file is resolved for ownership, not for its adapter. A provider
	// whose batches take their requests inline never receives the file at all,
	// so requiring its owner to serve files would refuse the very providers this
	// endpoint exists to reach (ADR 0021). What the batch needs from the file is
	// that this project owns it and Halro holds its bytes.
	principal, file, err := s.batchInputFile(ctx, key, call.InputFileID)
	if err != nil {
		return provider.BatchObject{}, err
	}
	batchTarget, err := s.ownedTarget(file)
	if err != nil {
		return provider.BatchObject{}, err
	}
	// An upstream that never received the file cannot be pointed at it, so the
	// requests travel with the batch. Reading them here rather than in the
	// adapter keeps the object directory the gateway's business: an adapter has
	// no idea where Halro puts its bytes, and should not learn.
	if file.UpstreamID == "" {
		call.ProviderModel = batchTarget.ProviderModel
		data, readErr := s.readResourceObject(file)
		if readErr != nil {
			return provider.BatchObject{}, gatewayError("resource_store_unavailable", "batch input could not be read", 503, readErr)
		}
		call.InputRequests = data
	}
	adapter, ok := batchTarget.Adapter.(provider.ResourceBatchesAdapter)
	if !ok {
		return provider.BatchObject{}, gatewayError("unsupported_feature", "batch adapter is unavailable", 400, nil)
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
	err = s.accountedInferenceResources(ctx, principal, file.PublicModel, target, 1, &requestID, func() error {
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
	record.InputFileID = file.ID
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

// nameBatchFiles replaces the upstream's file identifiers with Halro's own
// before a batch object is returned.
//
// The batch surface promises project-scoped opaque identifiers — the endpoint
// manifest says so in as many words — and it was keeping that promise for the
// batch itself and for nothing else. input_file_id, output_file_id and
// error_file_id went back to the caller exactly as the upstream wrote them,
// which leaked an upstream identifier and handed the caller something that
// cannot be used: Halro's own files endpoint resolves identifiers in the
// project's resource bucket, so an upstream file id answers 404 there. The
// documented way to collect a batch's results did not work.
//
// The input file is named from the record, because the caller supplied it and
// Halro already knows which one it was. The result files are named on first
// sight and remembered, because a batch is polled and minting a new identifier
// per poll would leave a trail of records for one upstream file.
//
// A record written before these fields existed carries none of them. Those
// batches answer with the fields absent rather than with the upstream's values:
// "not known here" is a worse answer than a correct one and a better answer
// than a wrong one.
// materialiseBatchResults turns an upstream's finished results into a file the
// caller can download, for providers that leave them somewhere rather than
// handing over a file.
//
// It runs while a batch is being read, not on a schedule. The fetch is bounded
// by the adapter's response ceiling, so it is bounded in time as well, which is
// what makes doing it inside a request acceptable — an unbounded fetch would
// need a background pass and a second writer this data directory does not have.
//
// Once stored, the identifier is remembered on the batch, so polling collects
// the same file rather than fetching it again.
func (s *Service) materialiseBatchResults(ctx context.Context, principal auth.AuthResult, resource domain.ProviderResource, result *provider.BatchObject) (domain.ProviderResource, error) {
	if resource.OutputFileID != "" || result.ResultsURL == "" {
		return resource, nil
	}
	target, err := s.ownedTarget(resource)
	if err != nil {
		return resource, err
	}
	fetcher, ok := target.Adapter.(provider.BatchResultsAdapter)
	if !ok {
		return resource, nil
	}
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var raw []byte
	if err := s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
		var fetchErr error
		raw, fetchErr = fetcher.FetchBatchResults(ctx, requestID, resource.UpstreamID, result.ResultsURL)
		return fetchErr
	}); err != nil {
		return resource, err
	}
	// Results are model output travelling outbound, and storing them writes a
	// response body outside its one-time response path. Every line goes through
	// the project's outbound policy before any of it reaches disk — redacting on
	// the way out instead would leave the unredacted copy sitting there.
	redacted, err := s.redactBatchResults(principal.Project.RedactionPolicyID, raw)
	if err != nil {
		return resource, gatewayError("sensitive_data_detected", "batch results contain material this project may not receive", 502, err)
	}
	fileID, err := s.storeBatchResults(ctx, resource, redacted)
	if err != nil {
		return resource, err
	}
	updated := resource
	updated.OutputFileID = fileID
	updated.UpdatedAt = s.now()
	stored, err := s.resources.PutProviderResource(ctx, updated, updated.Revision)
	if err != nil {
		// The file exists and can be named; only the memo failed. Returning the
		// identifier is better than failing a poll, and the next poll re-fetches
		// rather than losing it.
		result.OutputFileID = fileID
		return resource, nil
	}
	result.OutputFileID = fileID
	return stored, nil
}

func (s *Service) redactBatchResults(policyID string, raw []byte) ([]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var out bytes.Buffer
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		processed, err := s.redactor.ProcessJSON(policyID, "outbound", append(json.RawMessage(nil), line...))
		if err != nil {
			return nil, err
		}
		out.Write(processed)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// storeBatchResults writes the results as a local-only file owned by the same
// project and deployment as the batch. It has no upstream twin by construction:
// the bytes were assembled here.
func (s *Service) storeBatchResults(ctx context.Context, batch domain.ProviderResource, data []byte) (string, error) {
	externalID, err := id.New("file")
	if err != nil {
		return "", gatewayError("internal_error", "unable to create resource ID", 500, err)
	}
	objectPath, err := s.writeResourceObject(externalID, batch.ProjectID, data)
	if err != nil {
		return "", gatewayError("resource_store_unavailable", "batch results could not be stored", 503, err)
	}
	now := s.now()
	record := domain.ProviderResource{
		ID: externalID, Kind: domain.ResourceFile, ProjectID: batch.ProjectID,
		ProviderID: batch.ProviderID, DeploymentID: batch.DeploymentID, PublicModel: batch.PublicModel,
		ProfileID: batch.ProfileID, Region: batch.Region,
		ObjectPath: objectPath, ObjectBytes: int64(len(data)), ObjectContentType: "application/jsonl",
		ObjectFilename: externalID + ".jsonl", ObjectPurpose: "batch_output",
		CreationStatus: creationCompleted, Status: "uploaded",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: batch.ExpiresAt,
	}
	if _, err := s.resources.PutProviderResource(ctx, record, 0); err != nil {
		return "", gatewayError("resource_store_unavailable", "batch results could not be recorded", 503, err)
	}
	return externalID, nil
}

func (s *Service) nameBatchFiles(ctx context.Context, resource domain.ProviderResource, result *provider.BatchObject) (domain.ProviderResource, error) {
	result.ID = resource.ID
	result.InputFileID = resource.InputFileID
	updated := resource
	for _, file := range []struct {
		upstream string
		halro    *string
		out      *string
	}{
		{result.OutputFileID, &updated.OutputFileID, &result.OutputFileID},
		{result.ErrorFileID, &updated.ErrorFileID, &result.ErrorFileID},
	} {
		if file.upstream == "" {
			// The upstream has not produced this file. Anything already recorded
			// stays recorded; a batch does not un-produce a result.
			*file.out = *file.halro
			continue
		}
		if *file.halro == "" {
			registered, err := s.registerUpstreamFile(ctx, resource, file.upstream)
			if err != nil {
				return resource, err
			}
			*file.halro = registered
		}
		*file.out = *file.halro
	}
	if updated.OutputFileID == resource.OutputFileID && updated.ErrorFileID == resource.ErrorFileID {
		return resource, nil
	}
	updated.UpdatedAt = s.now()
	stored, err := s.resources.PutProviderResource(ctx, updated, updated.Revision)
	if err != nil {
		// The files exist and the caller can be told about them; only the memo
		// failed. Answering with the identifiers already minted is better than
		// failing a poll, and the next poll re-registers rather than losing them.
		return resource, nil
	}
	return stored, nil
}

// registerUpstreamFile gives an upstream file produced by a batch a Halro
// identity, so it can be addressed through the files endpoint like any other.
//
// It has no local object: Halro never uploaded these bytes and does not hold
// them. DownloadFile fetches them from the upstream on demand, under the
// adapter's existing response ceiling, which is why this record can be created
// without deciding anything about storing large results.
func (s *Service) registerUpstreamFile(ctx context.Context, batch domain.ProviderResource, upstreamID string) (string, error) {
	externalID, err := id.New("file")
	if err != nil {
		return "", gatewayError("internal_error", "unable to create resource ID", 500, err)
	}
	now := s.now()
	record := domain.ProviderResource{
		ID: externalID, Kind: domain.ResourceFile, ProjectID: batch.ProjectID,
		ProviderID: batch.ProviderID, DeploymentID: batch.DeploymentID, PublicModel: batch.PublicModel,
		ProfileID: batch.ProfileID, Region: batch.Region, UpstreamID: upstreamID,
		CreationStatus: creationCompleted, Status: "uploaded",
		CreatedAt: now, UpdatedAt: now,
		// The results outlive the batch record that names them, so this borrows
		// the batch's expiry rather than the file default: a file the caller can
		// no longer reach through any batch has nothing left to be reached by.
		ExpiresAt: batch.ExpiresAt,
	}
	if _, err := s.resources.PutProviderResource(ctx, record, 0); err != nil {
		return "", gatewayError("resource_store_unavailable", "batch result file could not be recorded", 503, err)
	}
	return externalID, nil
}

func (s *Service) batchOwner(ctx context.Context, key, idValue string) (auth.AuthResult, domain.ProviderResource, provider.ResourceBatchesAdapter, error) {
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
	adapter, ok := target.Adapter.(provider.ResourceBatchesAdapter)
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
	err = s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
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
	resource, err = s.resources.ProviderResource(ctx, principal.Project.ID, resource.ID)
	if err != nil {
		return provider.BatchObject{}, gatewayError("resource_store_unavailable", "batch owner could not be read", 503, err)
	}
	resource, err = s.materialiseBatchResults(ctx, principal, resource, &result)
	if err != nil {
		return provider.BatchObject{}, err
	}
	if _, err := s.nameBatchFiles(ctx, resource, &result); err != nil {
		return provider.BatchObject{}, err
	}
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
	err = s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
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
	resource, err = s.resources.ProviderResource(ctx, principal.Project.ID, resource.ID)
	if err != nil {
		return provider.BatchObject{}, gatewayError("resource_store_unavailable", "batch owner could not be read", 503, err)
	}
	if _, err := s.nameBatchFiles(ctx, resource, &result); err != nil {
		return provider.BatchObject{}, err
	}
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
	if !slices.Contains(principal.Project.AllowedModels, request.Model) {
		return provider.AsyncInvokeObject{}, gatewayError("model_not_allowed", "model is not allowed for this project", 403, nil)
	}
	targets := s.registry.ResolveCandidatesFor(request.Model, provider.OperationAsyncInvoke)
	if len(targets) != 1 {
		return provider.AsyncInvokeObject{}, gatewayError("ambiguous_resource_route", "async invocation requires exactly one eligible deployment", 409, nil)
	}
	target := targets[0]
	adapter, ok := target.Adapter.(provider.BedrockInferenceResourcesAdapter)
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
	err = s.accountedInferenceResources(ctx, principal, request.Model, target, int64(len(request.Prompt))/4+1, &requestID, func() error {
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
	adapter, ok := target.Adapter.(provider.BedrockInferenceResourcesAdapter)
	if !ok {
		return provider.AsyncInvokeObject{}, gatewayError("resource_owner_unavailable", "async owner adapter is unavailable", 409, nil)
	}
	target.FixedRequestMicrosUSD = 0
	requestID := ""
	var result provider.AsyncInvokeObject
	err = s.accountedInferenceResources(ctx, principal, resource.PublicModel, target, 1, &requestID, func() error {
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
	if _, ok := target.Adapter.(provider.BedrockInferenceResourcesAdapter); !ok {
		return provider.AsyncInvokeObject{}, gatewayError("resource_owner_unavailable", "async owner adapter is unavailable", 409, nil)
	}
	return provider.AsyncInvokeObject{}, gatewayError("provider_cancel_unsupported", "Amazon Bedrock Runtime does not provide cancellation for accepted async invocations", 409, nil)
}
