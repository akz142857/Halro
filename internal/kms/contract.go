// Package kms defines the cloud-neutral boundary between Heimdall core and a
// Key Management Service adapter. It owns no provider SDK, credentials,
// persistence, retry loop, Slot selection, or Vault behavior.
package kms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// ProtectedPayloadSize is the canonical HKMSKEY1 wire payload size.
	ProtectedPayloadSize = 112
	// MaxCiphertextBytes is the cloud-neutral storage/transport ceiling. Each
	// adapter must additionally enforce its provider's smaller native limit.
	MaxCiphertextBytes        = 64 << 10
	MaxKeyReferenceBytes      = 2048
	MaxAlgorithmBytes         = 128
	MaxBindingEntries         = 16
	MaxBindingKeyBytes        = 128
	MaxBindingValueBytes      = 1024
	MaxProviderRequestIDBytes = 256
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

type Operation string

const (
	OperationWrap   Operation = "wrap"
	OperationUnwrap Operation = "unwrap"
)

type Wrapper interface {
	Provider() string
	Wrap(context.Context, WrapRequest) (WrapResult, error)
	Unwrap(context.Context, UnwrapRequest) (UnwrapResult, error)
}

// BindingContext is provider-neutral, bounded, opaque authenticated context.
// Core constructs it from trusted configuration; adapters translate it to the
// provider-native AAD/Encryption Context representation where one exists.
type BindingContext map[string]string

type WrapRequest struct {
	KeyReference   string
	Algorithm      string
	Plaintext      []byte
	BindingContext BindingContext
}

func (r WrapRequest) Validate() error {
	return errors.Join(
		validateKeyReference(r.KeyReference),
		validateAlgorithm(r.Algorithm),
		validateProtectedPayload(r.Plaintext),
		validateBindingContext(r.BindingContext),
	)
}

func (r WrapRequest) Clone() WrapRequest {
	r.Plaintext = bytes.Clone(r.Plaintext)
	r.BindingContext = cloneBindingContext(r.BindingContext)
	return r
}

type WrapResult struct {
	Ciphertext        []byte
	ProviderRequestID string
}

func (r WrapResult) Validate() error {
	return errors.Join(
		validateCiphertext(r.Ciphertext),
		validateProviderRequestID(r.ProviderRequestID),
	)
}

func (r WrapResult) Clone() WrapResult {
	r.Ciphertext = bytes.Clone(r.Ciphertext)
	return r
}

type UnwrapRequest struct {
	KeyReference   string
	Algorithm      string
	Ciphertext     []byte
	BindingContext BindingContext
}

func (r UnwrapRequest) Validate() error {
	return errors.Join(
		validateKeyReference(r.KeyReference),
		validateAlgorithm(r.Algorithm),
		validateCiphertext(r.Ciphertext),
		validateBindingContext(r.BindingContext),
	)
}

func (r UnwrapRequest) Clone() UnwrapRequest {
	r.Ciphertext = bytes.Clone(r.Ciphertext)
	r.BindingContext = cloneBindingContext(r.BindingContext)
	return r
}

type UnwrapResult struct {
	Plaintext         []byte
	ProviderRequestID string
}

func (r UnwrapResult) Validate() error {
	return errors.Join(
		validateProtectedPayload(r.Plaintext),
		validateProviderRequestID(r.ProviderRequestID),
	)
}

func (r UnwrapResult) Clone() UnwrapResult {
	r.Plaintext = bytes.Clone(r.Plaintext)
	return r
}

func ValidateProvider(provider string) error {
	if !identifier.MatchString(provider) {
		return errors.New("KMS provider identifier is invalid")
	}
	return nil
}

func validateKeyReference(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > MaxKeyReferenceBytes {
		return fmt.Errorf("KMS key reference must contain 1 to %d bytes", MaxKeyReferenceBytes)
	}
	return nil
}

func validateAlgorithm(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > MaxAlgorithmBytes {
		return fmt.Errorf("KMS algorithm must contain 1 to %d bytes", MaxAlgorithmBytes)
	}
	return nil
}

func validateProtectedPayload(value []byte) error {
	if len(value) != ProtectedPayloadSize {
		return fmt.Errorf("KMS protected payload must contain exactly %d bytes", ProtectedPayloadSize)
	}
	return nil
}

func validateCiphertext(value []byte) error {
	if len(value) == 0 || len(value) > MaxCiphertextBytes {
		return fmt.Errorf("KMS ciphertext must contain 1 to %d bytes", MaxCiphertextBytes)
	}
	return nil
}

func validateBindingContext(binding BindingContext) error {
	if len(binding) > MaxBindingEntries {
		return fmt.Errorf("KMS binding context cannot contain more than %d entries", MaxBindingEntries)
	}
	for key, value := range binding {
		if strings.TrimSpace(key) == "" || len(key) > MaxBindingKeyBytes || len(value) > MaxBindingValueBytes {
			return errors.New("KMS binding context key or value exceeds its boundary")
		}
	}
	return nil
}

func validateProviderRequestID(value string) error {
	if len(value) > MaxProviderRequestIDBytes {
		return fmt.Errorf("KMS provider request ID cannot exceed %d bytes", MaxProviderRequestIDBytes)
	}
	return nil
}

func cloneBindingContext(binding BindingContext) BindingContext {
	if len(binding) == 0 {
		return nil
	}
	cloned := make(BindingContext, len(binding))
	for key, value := range binding {
		cloned[key] = value
	}
	return cloned
}

type ErrorClass string

const (
	ErrorTransient          ErrorClass = "kms_transient"
	ErrorThrottled          ErrorClass = "kms_throttled"
	ErrorIdentityNotReady   ErrorClass = "kms_identity_not_ready"
	ErrorPermissionDenied   ErrorClass = "kms_permission_denied"
	ErrorKeyUnavailable     ErrorClass = "kms_key_unavailable"
	ErrorConfigInvalid      ErrorClass = "kms_config_invalid"
	ErrorCiphertextInvalid  ErrorClass = "kms_ciphertext_invalid"
	ErrorPayloadInvalid     ErrorClass = "kms_payload_invalid"
	ErrorVaultMismatch      ErrorClass = "kms_vault_mismatch"
	ErrorAdapterUnavailable ErrorClass = "kms_adapter_unavailable"
)

type Error struct {
	Class      ErrorClass
	Provider   string
	Operation  Operation
	RetryAfter time.Duration
	cause      error
}

func NewError(class ErrorClass, provider string, operation Operation, retryAfter time.Duration, cause error) *Error {
	return &Error{Class: class, Provider: provider, Operation: operation, RetryAfter: retryAfter, cause: cause}
}

// Error deliberately excludes the provider-native cause, key reference,
// ciphertext, payload, credentials, and response body. Callers may inspect the
// cause with errors.Is/errors.As but must expose only this stable message.
func (e *Error) Error() string {
	if e == nil {
		return "KMS operation failed"
	}
	provider := e.Provider
	if ValidateProvider(provider) != nil {
		provider = "unknown"
	}
	operation := e.Operation
	if operation != OperationWrap && operation != OperationUnwrap {
		operation = "operation"
	}
	class := e.Class
	if !ValidErrorClass(class) {
		class = "kms_unclassified"
	}
	return fmt.Sprintf("KMS provider %q %s failed: %s", provider, operation, class)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) Validate() error {
	if e == nil {
		return errors.New("KMS error is nil")
	}
	if !ValidErrorClass(e.Class) {
		return errors.New("KMS error class is invalid")
	}
	if err := ValidateProvider(e.Provider); err != nil {
		return err
	}
	if e.Operation != OperationWrap && e.Operation != OperationUnwrap {
		return errors.New("KMS error operation is invalid")
	}
	if e.RetryAfter < 0 {
		return errors.New("KMS retry-after cannot be negative")
	}
	return nil
}

func ValidErrorClass(class ErrorClass) bool {
	switch class {
	case ErrorTransient, ErrorThrottled, ErrorIdentityNotReady, ErrorPermissionDenied,
		ErrorKeyUnavailable, ErrorConfigInvalid, ErrorCiphertextInvalid,
		ErrorPayloadInvalid, ErrorVaultMismatch, ErrorAdapterUnavailable:
		return true
	default:
		return false
	}
}

func Classify(err error) ErrorClass {
	var classified *Error
	if errors.As(err, &classified) && classified != nil && ValidErrorClass(classified.Class) {
		return classified.Class
	}
	return ErrorTransient
}

func Retryable(class ErrorClass) bool {
	switch class {
	case ErrorTransient, ErrorThrottled, ErrorIdentityNotReady:
		return true
	default:
		return false
	}
}
