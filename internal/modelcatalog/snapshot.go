package modelcatalog

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

const (
	MinReadableSchema           = 1
	MaxReadableSchema           = 1
	CapabilityDictionaryVersion = 1
	SignatureAlgorithmEd25519   = "ed25519"
)

// Snapshot is the shared wire schema for both the bundled catalog and signed
// remote catalogs. The bundled copy has no expiry or signature; remote copies
// must carry every temporal and integrity field.
type Snapshot struct {
	SchemaVersion               int             `json:"schema_version"`
	CatalogRevision             string          `json:"catalog_revision"`
	Sequence                    uint64          `json:"sequence"`
	GeneratedAt                 time.Time       `json:"generated_at,omitempty"`
	ExpiresAt                   time.Time       `json:"expires_at,omitempty"`
	CapabilityDictionaryVersion int             `json:"capability_dictionary_version"`
	Entries                     []SnapshotEntry `json:"entries"`
}

type SnapshotEntry struct {
	ProviderType domain.ProviderType         `json:"provider_type"`
	ProfileID    domain.ProviderProfileID    `json:"profile_id"`
	TargetKind   domain.DeploymentTargetKind `json:"target_kind"`
	Model        string                      `json:"model"`
	Region       string                      `json:"region,omitempty"`
	Capabilities domain.ProviderCapabilities `json:"capabilities"`
	Unsupported  []string                    `json:"unsupported,omitempty"`
	Revoked      bool                        `json:"revoked,omitempty"`
}

type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

type SignedSnapshot struct {
	Payload    Snapshot    `json:"payload"`
	Signatures []Signature `json:"signatures"`
}

type TrustRoot struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	NotBefore time.Time
	NotAfter  time.Time
}

// BundledSnapshot renders the reviewed Go catalog through exactly the same
// entry schema consumed by the remote reader. It deliberately has no temporal
// validity or signature because its integrity comes from the Halro artifact.
func BundledSnapshot() Snapshot {
	entries := make([]SnapshotEntry, 0, Builtin().Len())
	for _, entry := range Builtin().Entries() {
		entries = append(entries, SnapshotEntry{
			ProviderType: entry.Key.ProviderType, ProfileID: entry.Key.Profile,
			TargetKind: entry.Key.TargetKind,
			Model:      entry.Key.Model, Region: entry.Key.Region,
			Capabilities: entry.Capabilities, Unsupported: slices.Clone(entry.Unsupported),
		})
	}
	snapshot := Snapshot{
		SchemaVersion:               MinReadableSchema,
		CapabilityDictionaryVersion: CapabilityDictionaryVersion, Entries: entries,
	}
	revision, err := ComputeSnapshotRevision(snapshot)
	if err != nil {
		panic("modelcatalog: canonicalize bundled snapshot: " + err.Error())
	}
	snapshot.CatalogRevision = revision
	return snapshot
}

func canonicalSnapshot(snapshot Snapshot, includeRevision bool) ([]byte, error) {
	canonical := normalizedSnapshot(snapshot)
	if !includeRevision {
		canonical.CatalogRevision = ""
	}
	slices.SortFunc(canonical.Entries, func(left, right SnapshotEntry) int {
		return strings.Compare(snapshotEntryKey(left), snapshotEntryKey(right))
	})
	return json.Marshal(canonical)
}

func snapshotEntryKey(entry SnapshotEntry) string {
	return (Key{ProviderType: entry.ProviderType, Profile: entry.ProfileID, TargetKind: entry.TargetKind, Model: entry.Model, Region: entry.Region}).canonical()
}

func normalizedSnapshot(snapshot Snapshot) Snapshot {
	normalized := snapshot
	normalized.Entries = slices.Clone(snapshot.Entries)
	for index := range normalized.Entries {
		normalized.Entries[index].Unsupported = slices.Clone(normalized.Entries[index].Unsupported)
		slices.Sort(normalized.Entries[index].Unsupported)
		normalized.Entries[index].Unsupported = slices.Compact(normalized.Entries[index].Unsupported)
	}
	return normalized
}

func ComputeSnapshotRevision(snapshot Snapshot) (string, error) {
	payload, err := canonicalSnapshot(snapshot, false)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// PrepareSnapshot returns the exact payload bytes an isolated signer must
// sign. It handles no private key material: the returned bytes are passed to
// an external KMS/HSM or offline signer, then wrapped with its public signature.
func PrepareSnapshot(snapshot Snapshot) (Snapshot, []byte, error) {
	snapshot = normalizedSnapshot(snapshot)
	snapshot.CatalogRevision = ""
	revision, err := ComputeSnapshotRevision(snapshot)
	if err != nil {
		return Snapshot{}, nil, err
	}
	snapshot.CatalogRevision = revision
	payload, err := canonicalSnapshot(snapshot, true)
	if err != nil {
		return Snapshot{}, nil, err
	}
	return snapshot, payload, nil
}

// AssembleSignedSnapshot verifies an externally produced detached signature
// before wrapping it. This prevents a 64-byte Ed25519 private key, which is the
// same length as a signature, from ever being copied into a catalog envelope.
func AssembleSignedSnapshot(payload []byte, keyID, signatureValue string, roots []TrustRoot) (SignedSnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return SignedSnapshot{}, fmt.Errorf("decode canonical catalog payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return SignedSnapshot{}, errors.New("canonical catalog payload has trailing JSON values")
	}
	prepared, canonical, err := PrepareSnapshot(snapshot)
	if err != nil {
		return SignedSnapshot{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return SignedSnapshot{}, errors.New("catalog payload is not canonical")
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureValue))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return SignedSnapshot{}, errors.New("catalog signature is not a base64-encoded Ed25519 signature")
	}
	for _, root := range roots {
		if root.KeyID == keyID && len(root.PublicKey) == ed25519.PublicKeySize && ed25519.Verify(root.PublicKey, canonical, signature) {
			return SignedSnapshot{Payload: prepared, Signatures: []Signature{{
				KeyID: keyID, Algorithm: SignatureAlgorithmEd25519, Value: strings.TrimSpace(signatureValue),
			}}}, nil
		}
	}
	return SignedSnapshot{}, errors.New("catalog detached signature does not match the selected public trust root")
}

type VerifyOptions struct {
	Now            time.Time
	TrustRoots     []TrustRoot
	PinnedRevision string
	MaxEntries     int
	AllowExpired   bool
}

func DecodeAndVerifySignedSnapshot(payload []byte, options VerifyOptions) (Snapshot, *Catalog, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var signed SignedSnapshot
	if err := decoder.Decode(&signed); err != nil {
		return Snapshot{}, nil, fmt.Errorf("decode signed catalog: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Snapshot{}, nil, errors.New("signed catalog has trailing JSON values")
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	if options.MaxEntries <= 0 {
		return Snapshot{}, nil, errors.New("catalog max entries must be positive")
	}
	if err := validateRemoteSnapshot(signed.Payload, options); err != nil {
		return Snapshot{}, nil, err
	}
	canonical, err := canonicalSnapshot(signed.Payload, true)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if err := verifySignatures(canonical, signed.Signatures, options.TrustRoots, options.Now, signed.Payload.GeneratedAt, signed.Payload.ExpiresAt); err != nil {
		return Snapshot{}, nil, err
	}
	signed.Payload = normalizedSnapshot(signed.Payload)
	catalog, err := catalogFromSnapshot(signed.Payload)
	if err != nil {
		return Snapshot{}, nil, err
	}
	return signed.Payload, catalog, nil
}

func validateRemoteSnapshot(snapshot Snapshot, options VerifyOptions) error {
	if snapshot.SchemaVersion < MinReadableSchema || snapshot.SchemaVersion > MaxReadableSchema {
		return fmt.Errorf("catalog schema %d is outside readable range [%d,%d]", snapshot.SchemaVersion, MinReadableSchema, MaxReadableSchema)
	}
	if snapshot.CapabilityDictionaryVersion != CapabilityDictionaryVersion {
		return fmt.Errorf("catalog capability dictionary %d is incompatible with reader %d", snapshot.CapabilityDictionaryVersion, CapabilityDictionaryVersion)
	}
	if snapshot.Sequence == 0 {
		return errors.New("catalog sequence must be positive")
	}
	if snapshot.GeneratedAt.IsZero() || snapshot.ExpiresAt.IsZero() || !snapshot.ExpiresAt.After(snapshot.GeneratedAt) {
		return errors.New("catalog requires a valid generated_at/expires_at interval")
	}
	if snapshot.GeneratedAt.After(options.Now.Add(5 * time.Minute)) {
		return errors.New("catalog generated_at is in the future")
	}
	if !options.AllowExpired && !options.Now.Before(snapshot.ExpiresAt) {
		return errors.New("catalog is expired")
	}
	if len(snapshot.Entries) > options.MaxEntries {
		return fmt.Errorf("catalog has %d entries, limit %d", len(snapshot.Entries), options.MaxEntries)
	}
	revision, err := ComputeSnapshotRevision(snapshot)
	if err != nil {
		return err
	}
	if snapshot.CatalogRevision != revision {
		return errors.New("catalog revision does not match canonical payload")
	}
	if options.PinnedRevision != "" && options.PinnedRevision != revision {
		return fmt.Errorf("catalog revision %q does not match pinned revision", revision)
	}
	return nil
}

func verifySignatures(payload []byte, signatures []Signature, roots []TrustRoot, now, generatedAt, expiresAt time.Time) error {
	if len(signatures) == 0 {
		return errors.New("catalog has no signatures")
	}
	rootByID := make(map[string]TrustRoot, len(roots))
	for _, root := range roots {
		if root.KeyID == "" || len(root.PublicKey) != ed25519.PublicKeySize {
			continue
		}
		rootByID[root.KeyID] = root
	}
	for _, signature := range signatures {
		if signature.Algorithm != SignatureAlgorithmEd25519 {
			continue
		}
		root, ok := rootByID[signature.KeyID]
		if !ok || !root.NotBefore.IsZero() && (now.Before(root.NotBefore) || generatedAt.Before(root.NotBefore)) ||
			!root.NotAfter.IsZero() && (!now.Before(root.NotAfter) || expiresAt.After(root.NotAfter)) {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(signature.Value)
		if err == nil && ed25519.Verify(root.PublicKey, payload, decoded) {
			return nil
		}
	}
	return errors.New("catalog signature is not trusted or is invalid")
}

func catalogFromSnapshot(snapshot Snapshot) (*Catalog, error) {
	entries := make([]Entry, 0, len(snapshot.Entries))
	seen := make(map[string]struct{}, len(snapshot.Entries))
	for _, item := range snapshot.Entries {
		key := Key{ProviderType: item.ProviderType, Profile: item.ProfileID, TargetKind: item.TargetKind, Model: item.Model, Region: item.Region}
		if err := key.Validate(); err != nil {
			return nil, fmt.Errorf("remote catalog entry %q has invalid identity: %w", item.Model, err)
		}
		canonicalKey := key.canonical()
		if _, exists := seen[canonicalKey]; exists {
			return nil, fmt.Errorf("remote catalog has duplicate entry for %q", item.Model)
		}
		seen[canonicalKey] = struct{}{}
		if item.TargetKind == "" {
			return nil, fmt.Errorf("remote catalog entry %q requires target_kind", item.Model)
		}
		if item.Revoked {
			if item.Capabilities != (domain.ProviderCapabilities{}) || len(item.Unsupported) > 0 {
				return nil, fmt.Errorf("revoked catalog entry %q must not carry capability claims", item.Model)
			}
			continue
		}
		entries = append(entries, Entry{
			Key: key, Status: StatusPartial, Source: SourceSignedCatalog,
			Capabilities: item.Capabilities, Unsupported: slices.Clone(item.Unsupported),
		})
	}
	return New(entries...)
}

// MergeSnapshots overlays exact signed entries on the bundled catalog. A
// signed revocation removes an exact bundled key; it never removes by prefix.
func MergeSnapshots(snapshot Snapshot) (*Catalog, error) {
	if _, err := catalogFromSnapshot(snapshot); err != nil {
		return nil, err
	}
	entries := make(map[string]Entry, Builtin().Len()+len(snapshot.Entries))
	for _, entry := range Builtin().Entries() {
		entries[entry.Key.canonical()] = entry
	}
	for _, item := range snapshot.Entries {
		key := Key{ProviderType: item.ProviderType, Profile: item.ProfileID, TargetKind: item.TargetKind, Model: item.Model, Region: item.Region}
		if item.Revoked {
			delete(entries, key.canonical())
			continue
		}
		entries[key.canonical()] = Entry{Key: key, Status: StatusPartial, Source: SourceSignedCatalog, Capabilities: item.Capabilities, Unsupported: slices.Clone(item.Unsupported)}
	}
	merged := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		merged = append(merged, entry)
	}
	return New(merged...)
}

func defaultTargetKind(providerType domain.ProviderType, profile domain.ProviderProfileID) domain.DeploymentTargetKind {
	switch {
	case providerType == domain.ProviderAzureOpenAI:
		return domain.TargetAzureDeployment
	case providerType == domain.ProviderOpenAICompatible:
		return domain.TargetCustomEndpointModel
	case providerType == domain.ProviderBedrock && !domain.IsBedrockMantleProfile(profile):
		return domain.TargetBedrockFoundationModel
	default:
		return domain.TargetModelID
	}
}
