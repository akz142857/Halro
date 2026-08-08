// Package fakekms implements the provider-neutral kms.Wrapper contract for
// offline tests. It is never selected by production configuration.
package fakekms

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/kms"
)

const Provider = "fake-kms"

type Fault struct {
	Operation kms.Operation
	Call      int
	Delay     time.Duration
	Err       error
}

type Call struct {
	Operation kms.Operation
	Sequence  int
}

type Wrapper struct {
	mu             sync.Mutex
	aead           cipher.AEAD
	faults         []Fault
	calls          []Call
	operationCalls map[kms.Operation]int
}

func New(key []byte, faults ...Fault) (*Wrapper, error) {
	if len(key) != 32 {
		return nil, errors.New("fake KMS key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Wrapper{aead: aead, faults: append([]Fault(nil), faults...), operationCalls: make(map[kms.Operation]int)}, nil
}

func (w *Wrapper) Provider() string { return Provider }

func (w *Wrapper) Wrap(ctx context.Context, request kms.WrapRequest) (kms.WrapResult, error) {
	if err := request.Validate(); err != nil {
		return kms.WrapResult{}, kms.NewError(kms.ErrorConfigInvalid, Provider, kms.OperationWrap, 0, err)
	}
	sequence, fault := w.begin(kms.OperationWrap)
	if err := applyFault(ctx, kms.OperationWrap, fault); err != nil {
		return kms.WrapResult{}, err
	}
	nonce := make([]byte, w.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return kms.WrapResult{}, kms.NewError(kms.ErrorTransient, Provider, kms.OperationWrap, 0, err)
	}
	aad := canonicalAAD(request.KeyReference, request.Algorithm, request.BindingContext)
	ciphertext := append(nonce, w.aead.Seal(nil, nonce, request.Plaintext, aad)...)
	result := kms.WrapResult{Ciphertext: ciphertext, ProviderRequestID: fmt.Sprintf("fake-wrap-%06d", sequence)}
	if err := result.Validate(); err != nil {
		return kms.WrapResult{}, kms.NewError(kms.ErrorTransient, Provider, kms.OperationWrap, 0, err)
	}
	return result, nil
}

func (w *Wrapper) Unwrap(ctx context.Context, request kms.UnwrapRequest) (kms.UnwrapResult, error) {
	if err := request.Validate(); err != nil {
		return kms.UnwrapResult{}, kms.NewError(kms.ErrorConfigInvalid, Provider, kms.OperationUnwrap, 0, err)
	}
	sequence, fault := w.begin(kms.OperationUnwrap)
	if err := applyFault(ctx, kms.OperationUnwrap, fault); err != nil {
		return kms.UnwrapResult{}, err
	}
	if len(request.Ciphertext) < w.aead.NonceSize()+w.aead.Overhead() {
		return kms.UnwrapResult{}, kms.NewError(kms.ErrorCiphertextInvalid, Provider, kms.OperationUnwrap, 0, errors.New("ciphertext is truncated"))
	}
	nonce := request.Ciphertext[:w.aead.NonceSize()]
	sealed := request.Ciphertext[w.aead.NonceSize():]
	aad := canonicalAAD(request.KeyReference, request.Algorithm, request.BindingContext)
	plaintext, err := w.aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		return kms.UnwrapResult{}, kms.NewError(kms.ErrorCiphertextInvalid, Provider, kms.OperationUnwrap, 0, err)
	}
	result := kms.UnwrapResult{Plaintext: plaintext, ProviderRequestID: fmt.Sprintf("fake-unwrap-%06d", sequence)}
	if err := result.Validate(); err != nil {
		clear(plaintext)
		return kms.UnwrapResult{}, kms.NewError(kms.ErrorPayloadInvalid, Provider, kms.OperationUnwrap, 0, err)
	}
	return result, nil
}

func (w *Wrapper) Calls() []Call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Call(nil), w.calls...)
}

func (w *Wrapper) begin(operation kms.Operation) (int, *Fault) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.operationCalls[operation]++
	sequence := w.operationCalls[operation]
	w.calls = append(w.calls, Call{Operation: operation, Sequence: sequence})
	for index := range w.faults {
		fault := &w.faults[index]
		if fault.Operation == operation && fault.Call == sequence {
			copy := *fault
			return sequence, &copy
		}
	}
	return sequence, nil
}

func applyFault(ctx context.Context, operation kms.Operation, fault *Fault) error {
	if err := ctx.Err(); err != nil {
		return kms.NewError(kms.ErrorTransient, Provider, operation, 0, err)
	}
	if fault == nil {
		return nil
	}
	if fault.Delay > 0 {
		timer := time.NewTimer(fault.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return kms.NewError(kms.ErrorTransient, Provider, operation, 0, ctx.Err())
		case <-timer.C:
		}
	}
	if fault.Err != nil {
		var classified *kms.Error
		if errors.As(fault.Err, &classified) && classified.Validate() == nil {
			return fault.Err
		}
		return kms.NewError(kms.ErrorTransient, Provider, operation, 0, fault.Err)
	}
	return nil
}

func canonicalAAD(keyReference, algorithm string, binding kms.BindingContext) []byte {
	var buffer bytes.Buffer
	writeString(&buffer, Provider)
	writeString(&buffer, keyReference)
	writeString(&buffer, algorithm)
	keys := make([]string, 0, len(binding))
	for key := range binding {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		writeString(&buffer, key)
		writeString(&buffer, binding[key])
	}
	return buffer.Bytes()
}

func writeString(buffer *bytes.Buffer, value string) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	buffer.Write(size[:])
	buffer.WriteString(value)
}
