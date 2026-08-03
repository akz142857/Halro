package kms

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type contractWrapper struct{}

func (contractWrapper) Provider() string { return "contract-test" }
func (contractWrapper) Wrap(context.Context, WrapRequest) (WrapResult, error) {
	return WrapResult{}, nil
}
func (contractWrapper) Unwrap(context.Context, UnwrapRequest) (UnwrapResult, error) {
	return UnwrapResult{}, nil
}

var _ Wrapper = contractWrapper{}

func TestContractRequestAndResponseBoundaries(t *testing.T) {
	payload := bytes.Repeat([]byte{1}, ProtectedPayloadSize)
	binding := BindingContext{"instance": "instance-1", "slot": "slot-1"}
	wrap := WrapRequest{KeyReference: "opaque-key-reference", Algorithm: "provider-algorithm", Plaintext: payload, BindingContext: binding}
	if err := wrap.Validate(); err != nil {
		t.Fatal(err)
	}
	unwrap := UnwrapRequest{KeyReference: wrap.KeyReference, Algorithm: wrap.Algorithm, Ciphertext: []byte{1}, BindingContext: binding}
	if err := unwrap.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (WrapResult{Ciphertext: bytes.Repeat([]byte{1}, MaxCiphertextBytes), ProviderRequestID: strings.Repeat("r", MaxProviderRequestIDBytes)}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (UnwrapResult{Plaintext: payload}).Validate(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		err  error
	}{
		{name: "short protected payload", err: (WrapRequest{KeyReference: "key", Algorithm: "algorithm", Plaintext: payload[:len(payload)-1]}).Validate()},
		{name: "long protected payload", err: (UnwrapResult{Plaintext: append(bytes.Clone(payload), 0)}).Validate()},
		{name: "empty ciphertext", err: (UnwrapRequest{KeyReference: "key", Algorithm: "algorithm"}).Validate()},
		{name: "large ciphertext", err: (WrapResult{Ciphertext: make([]byte, MaxCiphertextBytes+1)}).Validate()},
		{name: "large key reference", err: (WrapRequest{KeyReference: strings.Repeat("k", MaxKeyReferenceBytes+1), Algorithm: "algorithm", Plaintext: payload}).Validate()},
		{name: "large algorithm", err: (WrapRequest{KeyReference: "key", Algorithm: strings.Repeat("a", MaxAlgorithmBytes+1), Plaintext: payload}).Validate()},
		{name: "large binding value", err: (WrapRequest{KeyReference: "key", Algorithm: "algorithm", Plaintext: payload, BindingContext: BindingContext{"key": strings.Repeat("v", MaxBindingValueBytes+1)}}).Validate()},
	}
	for _, test := range tests {
		if test.err == nil {
			t.Fatalf("%s was accepted", test.name)
		}
	}
}

func TestContractClonesMutableMaterial(t *testing.T) {
	request := WrapRequest{Plaintext: []byte{1, 2, 3}, BindingContext: BindingContext{"key": "value"}}
	clone := request.Clone()
	clone.Plaintext[0] = 9
	clone.BindingContext["key"] = "changed"
	if request.Plaintext[0] != 1 || request.BindingContext["key"] != "value" {
		t.Fatal("WrapRequest clone aliases caller material")
	}
	ciphertext := WrapResult{Ciphertext: []byte{1, 2, 3}}
	ciphertextClone := ciphertext.Clone()
	ciphertextClone.Ciphertext[0] = 9
	if ciphertext.Ciphertext[0] != 1 {
		t.Fatal("WrapResult clone aliases caller material")
	}
}

func TestTypedErrorTaxonomyIsStableAndSecretSafe(t *testing.T) {
	classes := []ErrorClass{
		ErrorTransient, ErrorThrottled, ErrorIdentityNotReady, ErrorPermissionDenied,
		ErrorKeyUnavailable, ErrorConfigInvalid, ErrorCiphertextInvalid,
		ErrorPayloadInvalid, ErrorVaultMismatch, ErrorAdapterUnavailable,
	}
	for _, class := range classes {
		err := NewError(class, "fake-kms", OperationUnwrap, 0, errors.New("secret-native-response"))
		if validationErr := err.Validate(); validationErr != nil {
			t.Fatalf("class %q: %v", class, validationErr)
		}
		if Classify(err) != class {
			t.Fatalf("classify(%q)=%q", class, Classify(err))
		}
		if strings.Contains(err.Error(), "secret-native-response") {
			t.Fatalf("class %q exposed provider cause: %s", class, err)
		}
	}
	for _, class := range []ErrorClass{ErrorTransient, ErrorThrottled, ErrorIdentityNotReady} {
		if !Retryable(class) {
			t.Fatalf("class %q should be retryable", class)
		}
	}
	for _, class := range []ErrorClass{ErrorPermissionDenied, ErrorKeyUnavailable, ErrorConfigInvalid, ErrorCiphertextInvalid, ErrorPayloadInvalid, ErrorVaultMismatch, ErrorAdapterUnavailable} {
		if Retryable(class) {
			t.Fatalf("class %q must fail fast", class)
		}
	}
	cause := context.DeadlineExceeded
	err := NewError(ErrorTransient, "fake-kms", OperationWrap, time.Second, cause)
	if !errors.Is(err, cause) {
		t.Fatal("typed error did not retain cause for internal inspection")
	}
	invalid := NewError("secret-class", "SECRET/PROVIDER", "secret-operation", 0, errors.New("secret-cause"))
	for _, secret := range []string{"secret-class", "SECRET/PROVIDER", "secret-operation", "secret-cause"} {
		if strings.Contains(invalid.Error(), secret) {
			t.Fatalf("invalid typed error leaked %q: %s", secret, invalid)
		}
	}
	for name, unknown := range map[string]error{
		"plain error":     errors.New("unclassified provider failure"),
		"nil error":       nil,
		"malformed typed": invalid,
	} {
		t.Run("fail closed "+name, func(t *testing.T) {
			class := Classify(unknown)
			if class != ErrorAdapterUnavailable || Retryable(class) {
				t.Fatalf("unknown error class=%q retryable=%v", class, Retryable(class))
			}
		})
	}
}
