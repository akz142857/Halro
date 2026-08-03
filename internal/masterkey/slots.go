package masterkey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/akz142857/Heimdall/internal/vault"
)

const (
	KeySlotDescriptorFormatVersion = 1
	maxKeySlots                    = 64
	maxWrappedKeyBytes             = 64 << 10
	maxProviderParameters          = 32
)

var (
	ErrInvalidDescriptor  = errors.New("invalid key slot descriptor")
	ErrSlotNotFound       = errors.New("key slot not found")
	ErrSlotExists         = errors.New("key slot already exists")
	ErrInvalidTransition  = errors.New("invalid key slot state transition")
	ErrSlotRevision       = errors.New("key slot revision conflict")
	ErrDescriptorRevision = errors.New("key slot descriptor revision conflict")
	ErrLastUsableSlot     = errors.New("cannot revoke the last usable verified key slot")
	ErrVaultKeyMismatch   = errors.New("candidate master key does not match the vault")
	keySlotIdentifier     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type KeySlotState string

const (
	KeySlotPending  KeySlotState = "pending"
	KeySlotActive   KeySlotState = "active"
	KeySlotRetiring KeySlotState = "retiring"
	KeySlotRevoked  KeySlotState = "revoked"
)

type KeySlotPurpose string

const (
	KeySlotPrimary  KeySlotPurpose = "primary"
	KeySlotRecovery KeySlotPurpose = "recovery"
)

type PendingKeySlot struct {
	ID                 string            `json:"id"`
	Provider           string            `json:"provider"`
	Purpose            KeySlotPurpose    `json:"purpose"`
	KeyReference       string            `json:"key_reference"`
	Algorithm          string            `json:"algorithm,omitempty"`
	ProviderParameters map[string]string `json:"provider_parameters,omitempty"`
	WrappedKey         []byte            `json:"wrapped_key"`
}

type KeySlot struct {
	ID                   string            `json:"id"`
	Provider             string            `json:"provider"`
	Purpose              KeySlotPurpose    `json:"purpose"`
	State                KeySlotState      `json:"state"`
	KeyReference         string            `json:"key_reference,omitempty"`
	Algorithm            string            `json:"algorithm,omitempty"`
	ProviderParameters   map[string]string `json:"provider_parameters,omitempty"`
	WrappedKey           []byte            `json:"wrapped_key,omitempty"`
	MasterKeyFingerprint string            `json:"master_key_fingerprint"`
	CreatedAt            time.Time         `json:"created_at"`
	VerifiedAt           *time.Time        `json:"verified_at,omitempty"`
	UpdatedAt            time.Time         `json:"updated_at"`
	Revision             uint64            `json:"revision"`
}

type KeySlotDescriptor struct {
	FormatVersion        uint8     `json:"format_version"`
	MasterKeyFingerprint string    `json:"master_key_fingerprint"`
	ActiveGeneration     uint64    `json:"active_generation"`
	Revision             uint64    `json:"revision"`
	Slots                []KeySlot `json:"slots"`
}

// SlotTransition is safe to use as Audit evidence. It intentionally excludes
// wrapped bytes, provider parameters, key references, and fingerprints.
type SlotTransition struct {
	SlotID             string         `json:"slot_id"`
	Purpose            KeySlotPurpose `json:"purpose"`
	From               KeySlotState   `json:"from,omitempty"`
	To                 KeySlotState   `json:"to"`
	SlotRevision       uint64         `json:"slot_revision"`
	DescriptorRevision uint64         `json:"descriptor_revision"`
	OccurredAt         time.Time      `json:"occurred_at"`
}

func (t SlotTransition) AuditAction() string {
	switch t.To {
	case KeySlotPending:
		return "security.master_key_slot.added"
	case KeySlotActive:
		return "security.master_key_slot.verified"
	case KeySlotRetiring:
		return "security.master_key_slot.retiring"
	case KeySlotRevoked:
		return "security.master_key_slot.revoked"
	default:
		return ""
	}
}

type SlotUnwrapper interface {
	Unwrap(context.Context, KeySlot) ([]byte, error)
}

type CandidateVerifier interface {
	VerifyCandidate(context.Context, []byte) error
}

func NewKeySlotDescriptor(masterKeyFingerprint string) (KeySlotDescriptor, error) {
	descriptor := KeySlotDescriptor{
		FormatVersion:        KeySlotDescriptorFormatVersion,
		MasterKeyFingerprint: masterKeyFingerprint,
		ActiveGeneration:     1,
		Revision:             1,
		Slots:                []KeySlot{},
	}
	if err := descriptor.Validate(); err != nil {
		return KeySlotDescriptor{}, err
	}
	return descriptor, nil
}

func MasterKeyFingerprint(key []byte) (string, error) {
	if len(key) != vault.MasterKeySize {
		return "", fmt.Errorf("master key must be exactly %d bytes", vault.MasterKeySize)
	}
	digest := sha256.Sum256(key)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (d KeySlotDescriptor) Validate() error {
	var problems []error
	if d.FormatVersion != KeySlotDescriptorFormatVersion {
		problems = append(problems, errors.New("unsupported format version"))
	}
	if !validKeyFingerprint(d.MasterKeyFingerprint) {
		problems = append(problems, errors.New("invalid master key fingerprint"))
	}
	if d.ActiveGeneration == 0 {
		problems = append(problems, errors.New("active generation must be positive"))
	}
	if d.Revision == 0 {
		problems = append(problems, errors.New("descriptor revision must be positive"))
	}
	if len(d.Slots) > maxKeySlots {
		problems = append(problems, fmt.Errorf("descriptor cannot contain more than %d slots", maxKeySlots))
	}
	seen := make(map[string]struct{}, len(d.Slots))
	for index, slot := range d.Slots {
		if _, exists := seen[slot.ID]; exists {
			problems = append(problems, fmt.Errorf("slot %q is duplicated", slot.ID))
		} else {
			seen[slot.ID] = struct{}{}
		}
		if err := validateKeySlot(slot, d.MasterKeyFingerprint); err != nil {
			problems = append(problems, fmt.Errorf("slot %d: %w", index, err))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDescriptor, err)
	}
	return nil
}

func (d KeySlotDescriptor) Clone() KeySlotDescriptor {
	cloned := d
	cloned.Slots = make([]KeySlot, len(d.Slots))
	for index := range d.Slots {
		cloned.Slots[index] = cloneKeySlot(d.Slots[index])
	}
	return cloned
}

func (d KeySlotDescriptor) ProductionReady() bool {
	if d.Validate() != nil {
		return false
	}
	primary, recovery := false, false
	for _, slot := range d.Slots {
		if slot.State != KeySlotActive || slot.VerifiedAt == nil || slot.MasterKeyFingerprint != d.MasterKeyFingerprint {
			continue
		}
		switch slot.Purpose {
		case KeySlotPrimary:
			primary = true
		case KeySlotRecovery:
			recovery = true
		}
	}
	return primary && recovery
}

// ValidateSuccessor independently verifies the COW publication boundary. It
// prevents a persistence caller from bypassing the state machine by fabricating
// a valid-looking descriptor with skipped states, removed audit metadata, or
// changed provider material.
func (d KeySlotDescriptor) ValidateSuccessor(previous KeySlotDescriptor) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if d.FormatVersion != previous.FormatVersion || d.MasterKeyFingerprint != previous.MasterKeyFingerprint ||
		d.ActiveGeneration != previous.ActiveGeneration || d.Revision != previous.Revision+1 {
		return fmt.Errorf("%w: descriptor identity, generation, or revision changed illegally", ErrInvalidDescriptor)
	}
	if len(d.Slots) < len(previous.Slots) || len(d.Slots) > len(previous.Slots)+1 {
		return fmt.Errorf("%w: successor must retain every slot and add at most one", ErrInvalidDescriptor)
	}
	previousByID := make(map[string]KeySlot, len(previous.Slots))
	for _, slot := range previous.Slots {
		previousByID[slot.ID] = slot
	}
	changes := 0
	for _, slot := range d.Slots {
		old, existed := previousByID[slot.ID]
		if !existed {
			if slot.State != KeySlotPending || slot.Revision != 1 || len(d.Slots) != len(previous.Slots)+1 {
				return fmt.Errorf("%w: new slot must begin pending at revision 1", ErrInvalidDescriptor)
			}
			changes++
			continue
		}
		delete(previousByID, slot.ID)
		if keySlotsEqual(old, slot) {
			continue
		}
		changes++
		if err := validateSlotSuccessor(old, slot); err != nil {
			return err
		}
		if slot.State == KeySlotRetiring && !previous.canRetire(slot.ID) {
			return ErrLastUsableSlot
		}
		if slot.State == KeySlotRevoked && !previous.canRevoke(slot.ID) {
			return ErrLastUsableSlot
		}
	}
	if len(previousByID) != 0 {
		return fmt.Errorf("%w: successor removed existing slot metadata", ErrInvalidDescriptor)
	}
	if changes != 1 {
		return fmt.Errorf("%w: successor must contain exactly one logical slot change", ErrInvalidDescriptor)
	}
	return nil
}

func (d KeySlotDescriptor) AddSlot(pending PendingKeySlot, expectedDescriptorRevision uint64, now time.Time) (KeySlotDescriptor, *SlotTransition, error) {
	if err := d.Validate(); err != nil {
		return KeySlotDescriptor{}, nil, err
	}
	if err := validatePendingSlot(pending); err != nil {
		return KeySlotDescriptor{}, nil, err
	}
	if existing, ok := d.slot(pending.ID); ok {
		if existing.State == KeySlotPending && pendingMatches(existing, pending) {
			unchanged := d.Clone()
			return unchanged, nil, nil
		}
		return KeySlotDescriptor{}, nil, ErrSlotExists
	}
	if d.Revision != expectedDescriptorRevision {
		return KeySlotDescriptor{}, nil, ErrDescriptorRevision
	}
	if len(d.Slots) >= maxKeySlots {
		return KeySlotDescriptor{}, nil, fmt.Errorf("%w: slot limit reached", ErrInvalidDescriptor)
	}
	now = now.UTC()
	if now.IsZero() {
		return KeySlotDescriptor{}, nil, errors.New("slot transition time is required")
	}
	next := d.Clone()
	slot := KeySlot{
		ID: pending.ID, Provider: pending.Provider, Purpose: pending.Purpose, State: KeySlotPending,
		KeyReference: pending.KeyReference, Algorithm: pending.Algorithm,
		ProviderParameters: cloneParameters(pending.ProviderParameters), WrappedKey: bytes.Clone(pending.WrappedKey),
		MasterKeyFingerprint: d.MasterKeyFingerprint, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	next.Slots = append(next.Slots, slot)
	sort.Slice(next.Slots, func(i, j int) bool { return next.Slots[i].ID < next.Slots[j].ID })
	next.Revision++
	transition := &SlotTransition{
		SlotID: slot.ID, Purpose: slot.Purpose, To: KeySlotPending, SlotRevision: slot.Revision,
		DescriptorRevision: next.Revision, OccurredAt: now,
	}
	return next, transition, next.Validate()
}

func (d KeySlotDescriptor) VerifySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	unwrapper SlotUnwrapper,
	verifier CandidateVerifier,
	now time.Time,
) (KeySlotDescriptor, *SlotTransition, error) {
	if err := d.Validate(); err != nil {
		return KeySlotDescriptor{}, nil, err
	}
	slot, ok := d.slot(slotID)
	if !ok {
		return KeySlotDescriptor{}, nil, ErrSlotNotFound
	}
	if slot.State == KeySlotActive {
		return d.Clone(), nil, nil
	}
	if slot.State != KeySlotPending {
		return KeySlotDescriptor{}, nil, ErrInvalidTransition
	}
	if d.Revision != expectedDescriptorRevision {
		return KeySlotDescriptor{}, nil, ErrDescriptorRevision
	}
	if slot.Revision != expectedSlotRevision {
		return KeySlotDescriptor{}, nil, ErrSlotRevision
	}
	if unwrapper == nil || verifier == nil {
		return KeySlotDescriptor{}, nil, errors.New("slot unwrapper and candidate verifier are required")
	}
	if err := ctx.Err(); err != nil {
		return KeySlotDescriptor{}, nil, err
	}
	candidate, err := unwrapper.Unwrap(ctx, cloneKeySlot(slot))
	if err != nil {
		return KeySlotDescriptor{}, nil, fmt.Errorf("unwrap key slot: %w", err)
	}
	defer clear(candidate)
	fingerprint, err := MasterKeyFingerprint(candidate)
	if err != nil || !fingerprintsEqual(fingerprint, d.MasterKeyFingerprint) {
		return KeySlotDescriptor{}, nil, ErrVaultKeyMismatch
	}
	if err := verifier.VerifyCandidate(ctx, candidate); err != nil {
		return KeySlotDescriptor{}, nil, fmt.Errorf("%w: %v", ErrVaultKeyMismatch, err)
	}
	now = now.UTC()
	if now.IsZero() {
		return KeySlotDescriptor{}, nil, errors.New("slot transition time is required")
	}
	next := d.Clone()
	index := next.slotIndex(slotID)
	previous := next.Slots[index].State
	next.Slots[index].State = KeySlotActive
	next.Slots[index].VerifiedAt = timePointer(now)
	next.Slots[index].UpdatedAt = now
	next.Slots[index].Revision++
	next.Revision++
	transition := &SlotTransition{
		SlotID: slotID, Purpose: slot.Purpose, From: previous, To: KeySlotActive,
		SlotRevision: next.Slots[index].Revision, DescriptorRevision: next.Revision, OccurredAt: now,
	}
	return next, transition, next.Validate()
}

func (d KeySlotDescriptor) RetireSlot(slotID string, expectedDescriptorRevision, expectedSlotRevision uint64, now time.Time) (KeySlotDescriptor, *SlotTransition, error) {
	return d.transitionSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, KeySlotRetiring, now)
}

func (d KeySlotDescriptor) RevokeSlot(slotID string, expectedDescriptorRevision, expectedSlotRevision uint64, now time.Time) (KeySlotDescriptor, *SlotTransition, error) {
	return d.transitionSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, KeySlotRevoked, now)
}

func (d KeySlotDescriptor) transitionSlot(slotID string, expectedDescriptorRevision, expectedSlotRevision uint64, target KeySlotState, now time.Time) (KeySlotDescriptor, *SlotTransition, error) {
	if err := d.Validate(); err != nil {
		return KeySlotDescriptor{}, nil, err
	}
	slot, ok := d.slot(slotID)
	if !ok {
		return KeySlotDescriptor{}, nil, ErrSlotNotFound
	}
	if slot.State == target {
		return d.Clone(), nil, nil
	}
	valid := (slot.State == KeySlotActive && target == KeySlotRetiring) ||
		(slot.State == KeySlotRetiring && target == KeySlotRevoked)
	if !valid {
		return KeySlotDescriptor{}, nil, ErrInvalidTransition
	}
	if d.Revision != expectedDescriptorRevision {
		return KeySlotDescriptor{}, nil, ErrDescriptorRevision
	}
	if slot.Revision != expectedSlotRevision {
		return KeySlotDescriptor{}, nil, ErrSlotRevision
	}
	if target == KeySlotRetiring && !d.canRetire(slotID) {
		return KeySlotDescriptor{}, nil, ErrLastUsableSlot
	}
	if target == KeySlotRevoked && !d.canRevoke(slotID) {
		return KeySlotDescriptor{}, nil, ErrLastUsableSlot
	}
	now = now.UTC()
	if now.IsZero() {
		return KeySlotDescriptor{}, nil, errors.New("slot transition time is required")
	}
	next := d.Clone()
	index := next.slotIndex(slotID)
	previous := next.Slots[index].State
	next.Slots[index].State = target
	next.Slots[index].UpdatedAt = now
	next.Slots[index].Revision++
	if target == KeySlotRevoked {
		clear(next.Slots[index].WrappedKey)
		next.Slots[index].WrappedKey = nil
		next.Slots[index].KeyReference = ""
		next.Slots[index].Algorithm = ""
		next.Slots[index].ProviderParameters = nil
	}
	next.Revision++
	transition := &SlotTransition{
		SlotID: slotID, Purpose: slot.Purpose, From: previous, To: target,
		SlotRevision: next.Slots[index].Revision, DescriptorRevision: next.Revision, OccurredAt: now,
	}
	return next, transition, next.Validate()
}

func validatePendingSlot(slot PendingKeySlot) error {
	var problems []error
	if !keySlotIdentifier.MatchString(slot.ID) {
		problems = append(problems, errors.New("slot ID is invalid"))
	}
	if !keySlotIdentifier.MatchString(slot.Provider) {
		problems = append(problems, errors.New("provider is invalid"))
	}
	if slot.Purpose != KeySlotPrimary && slot.Purpose != KeySlotRecovery {
		problems = append(problems, errors.New("purpose must be primary or recovery"))
	}
	if len(slot.KeyReference) == 0 || len(slot.KeyReference) > 2048 {
		problems = append(problems, errors.New("key reference must contain 1 to 2048 bytes"))
	}
	if len(slot.Algorithm) > 128 {
		problems = append(problems, errors.New("algorithm cannot exceed 128 bytes"))
	}
	if len(slot.WrappedKey) == 0 || len(slot.WrappedKey) > maxWrappedKeyBytes {
		problems = append(problems, fmt.Errorf("wrapped key must contain 1 to %d bytes", maxWrappedKeyBytes))
	}
	if err := validateProviderParameters(slot.ProviderParameters); err != nil {
		problems = append(problems, err)
	}
	if err := errors.Join(problems...); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDescriptor, err)
	}
	return nil
}

func validateKeySlot(slot KeySlot, descriptorFingerprint string) error {
	pending := PendingKeySlot{
		ID: slot.ID, Provider: slot.Provider, Purpose: slot.Purpose, KeyReference: slot.KeyReference,
		Algorithm: slot.Algorithm, ProviderParameters: slot.ProviderParameters, WrappedKey: slot.WrappedKey,
	}
	if slot.State == KeySlotRevoked {
		pending.KeyReference = "revoked"
		pending.WrappedKey = []byte{1}
		pending.ProviderParameters = nil
	}
	var problems []error
	if err := validatePendingSlot(pending); err != nil {
		problems = append(problems, err)
	}
	if slot.State != KeySlotPending && slot.State != KeySlotActive && slot.State != KeySlotRetiring && slot.State != KeySlotRevoked {
		problems = append(problems, errors.New("state is invalid"))
	}
	if slot.MasterKeyFingerprint != descriptorFingerprint {
		problems = append(problems, errors.New("slot fingerprint does not match descriptor"))
	}
	if slot.CreatedAt.IsZero() || slot.CreatedAt.Location() != time.UTC || slot.UpdatedAt.IsZero() || slot.UpdatedAt.Location() != time.UTC || slot.UpdatedAt.Before(slot.CreatedAt) {
		problems = append(problems, errors.New("slot timestamps are invalid"))
	}
	if slot.Revision == 0 {
		problems = append(problems, errors.New("slot revision must be positive"))
	}
	switch slot.State {
	case KeySlotPending:
		if slot.VerifiedAt != nil {
			problems = append(problems, errors.New("pending slot cannot be verified"))
		}
	case KeySlotActive, KeySlotRetiring:
		if slot.VerifiedAt == nil || slot.VerifiedAt.IsZero() || slot.VerifiedAt.Location() != time.UTC {
			problems = append(problems, errors.New("verified slot requires a UTC verification time"))
		} else if slot.VerifiedAt.Before(slot.CreatedAt) || slot.VerifiedAt.After(slot.UpdatedAt) {
			problems = append(problems, errors.New("verification time must be within the slot lifetime"))
		}
	case KeySlotRevoked:
		if slot.VerifiedAt == nil || len(slot.WrappedKey) != 0 || slot.KeyReference != "" || slot.Algorithm != "" || len(slot.ProviderParameters) != 0 {
			problems = append(problems, errors.New("revoked slot must retain only non-sensitive audit metadata"))
		} else if slot.VerifiedAt.Before(slot.CreatedAt) || slot.VerifiedAt.After(slot.UpdatedAt) {
			problems = append(problems, errors.New("verification time must be within the slot lifetime"))
		}
	}
	return errors.Join(problems...)
}

func validateProviderParameters(parameters map[string]string) error {
	if len(parameters) > maxProviderParameters {
		return fmt.Errorf("provider parameters cannot contain more than %d entries", maxProviderParameters)
	}
	for key, value := range parameters {
		if !keySlotIdentifier.MatchString(key) || len(value) > 2048 {
			return errors.New("provider parameter key or value is invalid")
		}
	}
	return nil
}

func validKeyFingerprint(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	_, err := hex.DecodeString(value[len("sha256:"):])
	return err == nil
}

func fingerprintsEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func cloneKeySlot(slot KeySlot) KeySlot {
	cloned := slot
	cloned.WrappedKey = bytes.Clone(slot.WrappedKey)
	cloned.ProviderParameters = cloneParameters(slot.ProviderParameters)
	if slot.VerifiedAt != nil {
		verified := *slot.VerifiedAt
		cloned.VerifiedAt = &verified
	}
	return cloned
}

func cloneParameters(parameters map[string]string) map[string]string {
	if len(parameters) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(parameters))
	for key, value := range parameters {
		cloned[key] = value
	}
	return cloned
}

func pendingMatches(existing KeySlot, pending PendingKeySlot) bool {
	return existing.ID == pending.ID && existing.Provider == pending.Provider && existing.Purpose == pending.Purpose &&
		existing.KeyReference == pending.KeyReference && existing.Algorithm == pending.Algorithm &&
		bytes.Equal(existing.WrappedKey, pending.WrappedKey) && mapsEqual(existing.ProviderParameters, pending.ProviderParameters)
}

func validateSlotSuccessor(previous, next KeySlot) error {
	if next.Revision != previous.Revision+1 || next.ID != previous.ID || next.Provider != previous.Provider ||
		next.Purpose != previous.Purpose || next.MasterKeyFingerprint != previous.MasterKeyFingerprint ||
		!next.CreatedAt.Equal(previous.CreatedAt) || !next.UpdatedAt.After(previous.UpdatedAt) {
		return fmt.Errorf("%w: slot immutable fields or revision changed illegally", ErrInvalidDescriptor)
	}
	valid := (previous.State == KeySlotPending && next.State == KeySlotActive) ||
		(previous.State == KeySlotActive && next.State == KeySlotRetiring) ||
		(previous.State == KeySlotRetiring && next.State == KeySlotRevoked)
	if !valid {
		return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, previous.State, next.State)
	}
	if next.State == KeySlotRevoked {
		if next.KeyReference != "" || next.Algorithm != "" || len(next.ProviderParameters) != 0 || len(next.WrappedKey) != 0 ||
			!timesEqual(previous.VerifiedAt, next.VerifiedAt) {
			return fmt.Errorf("%w: revoked slot retained protected material or changed verification time", ErrInvalidDescriptor)
		}
		return nil
	}
	if next.KeyReference != previous.KeyReference || next.Algorithm != previous.Algorithm ||
		!mapsEqual(next.ProviderParameters, previous.ProviderParameters) || !bytes.Equal(next.WrappedKey, previous.WrappedKey) {
		return fmt.Errorf("%w: slot provider material changed outside replacement flow", ErrInvalidDescriptor)
	}
	if previous.State == KeySlotPending {
		if previous.VerifiedAt != nil || next.VerifiedAt == nil {
			return fmt.Errorf("%w: pending verification time is invalid", ErrInvalidDescriptor)
		}
	} else if !timesEqual(previous.VerifiedAt, next.VerifiedAt) {
		return fmt.Errorf("%w: verification time changed after activation", ErrInvalidDescriptor)
	}
	return nil
}

func keySlotsEqual(left, right KeySlot) bool {
	return left.ID == right.ID && left.Provider == right.Provider && left.Purpose == right.Purpose &&
		left.State == right.State && left.KeyReference == right.KeyReference && left.Algorithm == right.Algorithm &&
		mapsEqual(left.ProviderParameters, right.ProviderParameters) && bytes.Equal(left.WrappedKey, right.WrappedKey) &&
		left.MasterKeyFingerprint == right.MasterKeyFingerprint && left.CreatedAt.Equal(right.CreatedAt) &&
		timesEqual(left.VerifiedAt, right.VerifiedAt) && left.UpdatedAt.Equal(right.UpdatedAt) && left.Revision == right.Revision
}

func timesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func (d KeySlotDescriptor) slot(id string) (KeySlot, bool) {
	index := d.slotIndex(id)
	if index < 0 {
		return KeySlot{}, false
	}
	return cloneKeySlot(d.Slots[index]), true
}

func (d KeySlotDescriptor) slotIndex(id string) int {
	for index := range d.Slots {
		if d.Slots[index].ID == id {
			return index
		}
	}
	return -1
}

func (d KeySlotDescriptor) canRetire(id string) bool {
	wasProductionReady := d.ProductionReady()
	activePrimary, activeRecovery, activeTotal := 0, 0, 0
	for _, slot := range d.Slots {
		if slot.ID == id || slot.State != KeySlotActive {
			continue
		}
		activeTotal++
		if slot.Purpose == KeySlotPrimary {
			activePrimary++
		} else if slot.Purpose == KeySlotRecovery {
			activeRecovery++
		}
	}
	if activeTotal == 0 {
		return false
	}
	return !wasProductionReady || (activePrimary > 0 && activeRecovery > 0)
}

func (d KeySlotDescriptor) canRevoke(id string) bool {
	usable := 0
	for _, slot := range d.Slots {
		if slot.ID != id && slot.VerifiedAt != nil && (slot.State == KeySlotActive || slot.State == KeySlotRetiring) {
			usable++
		}
	}
	return usable > 0
}

func timePointer(value time.Time) *time.Time {
	return &value
}
