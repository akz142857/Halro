package awskms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corekms "github.com/akz142857/Heimdall/internal/kms"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	servicekms "github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var _ corekms.Wrapper = (*Wrapper)(nil)

type mockClient struct {
	encryptInput  *servicekms.EncryptInput
	decryptInput  *servicekms.DecryptInput
	encryptOutput *servicekms.EncryptOutput
	decryptOutput *servicekms.DecryptOutput
	encryptErr    error
	decryptErr    error
}

func (c *mockClient) Encrypt(_ context.Context, input *servicekms.EncryptInput, _ ...func(*servicekms.Options)) (*servicekms.EncryptOutput, error) {
	c.encryptInput = input
	return c.encryptOutput, c.encryptErr
}

func (c *mockClient) Decrypt(_ context.Context, input *servicekms.DecryptInput, _ ...func(*servicekms.Options)) (*servicekms.DecryptOutput, error) {
	c.decryptInput = input
	return c.decryptOutput, c.decryptErr
}

func TestAdapterWrapUnwrapUsesExactAWSBindingAndAllowlist(t *testing.T) {
	options := testOptions()
	binding, err := NewBindingContext(corekms.PayloadBinding{InstanceID: "instance-1", SlotID: "slot_primary"}, EncryptionContextPurposePrimary)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := corekms.EncodeProtectedPayload(corekms.PayloadBinding{InstanceID: "instance-1", SlotID: "slot_primary"}, bytes.Repeat([]byte{7}, corekms.MasterKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	encryptMetadata := middleware.Metadata{}
	awsmiddleware.SetRequestIDMetadata(&encryptMetadata, "aws-encrypt-request")
	decryptMetadata := middleware.Metadata{}
	awsmiddleware.SetRequestIDMetadata(&decryptMetadata, "aws-decrypt-request")
	decryptPlaintext := bytes.Clone(payload)
	client := &mockClient{
		encryptOutput: &servicekms.EncryptOutput{
			CiphertextBlob: []byte("aws-ciphertext"), KeyId: aws.String(options.KeyARN),
			EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, ResultMetadata: encryptMetadata,
		},
		decryptOutput: &servicekms.DecryptOutput{
			Plaintext: decryptPlaintext, KeyId: aws.String(options.KeyARN),
			EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, ResultMetadata: decryptMetadata,
		},
	}
	wrapper, err := newWithClient(options, client)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapper.Wrap(context.Background(), corekms.WrapRequest{
		KeyReference: options.KeyARN, Algorithm: options.Algorithm, Plaintext: payload, BindingContext: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.ProviderRequestID != "aws-encrypt-request" || string(wrapped.Ciphertext) != "aws-ciphertext" {
		t.Fatalf("wrapped=%#v", wrapped)
	}
	if client.encryptInput == nil || aws.ToString(client.encryptInput.KeyId) != options.KeyARN ||
		client.encryptInput.EncryptionAlgorithm != types.EncryptionAlgorithmSpecSymmetricDefault ||
		!mapsEqualForAWSTest(client.encryptInput.EncryptionContext, binding) {
		t.Fatalf("encrypt input=%#v", client.encryptInput)
	}
	if strings.Contains(strings.Join(mapValues(binding), " "), "instance-1") || strings.Contains(strings.Join(mapValues(binding), " "), "slot_primary") {
		t.Fatalf("AWS Encryption Context exposed raw identifiers: %#v", binding)
	}
	unwrapped, err := wrapper.Unwrap(context.Background(), corekms.UnwrapRequest{
		KeyReference: options.KeyARN, Algorithm: options.Algorithm, Ciphertext: wrapped.Ciphertext, BindingContext: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(unwrapped.Plaintext)
	if unwrapped.ProviderRequestID != "aws-decrypt-request" || !bytes.Equal(unwrapped.Plaintext, payload) {
		t.Fatalf("unwrapped=%#v", unwrapped)
	}
	if !allZeroAWS(decryptPlaintext) {
		t.Fatal("AWS SDK plaintext response was not cleared after ownership transfer")
	}
	if client.decryptInput == nil || aws.ToString(client.decryptInput.KeyId) != options.KeyARN ||
		!mapsEqualForAWSTest(client.decryptInput.EncryptionContext, binding) {
		t.Fatalf("decrypt input=%#v", client.decryptInput)
	}
}

func TestOfficialAWSSDKEncryptDecryptWireContractAndContextMismatch(t *testing.T) {
	options := testOptions()
	binding, err := NewBindingContext(corekms.PayloadBinding{InstanceID: "instance-1", SlotID: "slot_primary"}, EncryptionContextPurposePrimary)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{7}, corekms.ProtectedPayloadSize)
	ciphertext := []byte("official-sdk-ciphertext")
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "" || request.Header.Get("X-Amz-Target") == "" {
			t.Error("official SDK request was not signed or targeted")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			KeyID             string            `json:"KeyId"`
			EncryptionContext map[string]string `json:"EncryptionContext"`
			CiphertextBlob    string            `json:"CiphertextBlob"`
			Plaintext         string            `json:"Plaintext"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.KeyID != options.KeyARN {
			t.Errorf("wire KeyId=%q", body.KeyID)
		}
		response.Header().Set("Content-Type", "application/x-amz-json-1.1")
		response.Header().Set("x-amzn-RequestId", "wire-request-id")
		switch request.Header.Get("X-Amz-Target") {
		case "TrentService.Encrypt":
			if !mapsEqualForAWSTest(body.EncryptionContext, binding) || body.Plaintext != base64.StdEncoding.EncodeToString(payload) {
				t.Errorf("unexpected Encrypt wire body: %#v", body)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"CiphertextBlob": base64.StdEncoding.EncodeToString(ciphertext),
				"KeyId":          options.KeyARN, "EncryptionAlgorithm": SymmetricDefaultAlgorithm,
			})
		case "TrentService.Decrypt":
			if !mapsEqualForAWSTest(body.EncryptionContext, binding) {
				response.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(response).Encode(map[string]any{"__type": "InvalidCiphertextException", "message": "context mismatch secret"})
				return
			}
			if body.CiphertextBlob != base64.StdEncoding.EncodeToString(ciphertext) {
				t.Errorf("unexpected Decrypt ciphertext: %q", body.CiphertextBlob)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"Plaintext": base64.StdEncoding.EncodeToString(payload),
				"KeyId":     options.KeyARN, "EncryptionAlgorithm": SymmetricDefaultAlgorithm,
			})
		default:
			response.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	options.Endpoint = server.URL
	awsConfig := aws.Config{
		Region: options.Region, Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("temporary", "temporary", "temporary")),
		HTTPClient: server.Client(), Retryer: func() aws.Retryer { return aws.NopRetryer{} },
	}
	client := servicekms.NewFromConfig(awsConfig, func(value *servicekms.Options) { value.BaseEndpoint = aws.String(server.URL) })
	wrapper, err := newWithClient(options, client)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapper.Wrap(context.Background(), corekms.WrapRequest{
		KeyReference: options.KeyARN, Algorithm: options.Algorithm, Plaintext: payload, BindingContext: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.ProviderRequestID != "wire-request-id" || !bytes.Equal(wrapped.Ciphertext, ciphertext) {
		t.Fatalf("wrapped=%#v", wrapped)
	}
	unwrapped, err := wrapper.Unwrap(context.Background(), corekms.UnwrapRequest{
		KeyReference: options.KeyARN, Algorithm: options.Algorithm, Ciphertext: wrapped.Ciphertext, BindingContext: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	clear(unwrapped.Plaintext)
	tampered := cloneContextForAWSTest(binding)
	tampered[contextKeySlotSHA256] = strings.Repeat("0", 64)
	_, err = wrapper.Unwrap(context.Background(), corekms.UnwrapRequest{
		KeyReference: options.KeyARN, Algorithm: options.Algorithm, Ciphertext: wrapped.Ciphertext, BindingContext: tampered,
	})
	if corekms.Classify(err) != corekms.ErrorCiphertextInvalid || strings.Contains(err.Error(), "context mismatch secret") {
		t.Fatalf("context mismatch class=%q err=%v", corekms.Classify(err), err)
	}
}

func TestAdapterRejectsRequestsOutsideAllowlistBeforeAWSCall(t *testing.T) {
	options := testOptions()
	binding, err := NewBindingContext(corekms.PayloadBinding{InstanceID: "instance-1", SlotID: "slot_primary"}, EncryptionContextPurposePrimary)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{1}, corekms.ProtectedPayloadSize)
	tests := []struct {
		name   string
		wrap   corekms.WrapRequest
		unwrap corekms.UnwrapRequest
		class  corekms.ErrorClass
	}{
		{name: "unknown key", wrap: corekms.WrapRequest{KeyReference: strings.Replace(options.KeyARN, "1234", "9999", 1), Algorithm: options.Algorithm, Plaintext: payload, BindingContext: binding}, class: corekms.ErrorConfigInvalid},
		{name: "unknown algorithm", wrap: corekms.WrapRequest{KeyReference: options.KeyARN, Algorithm: "RSAES_OAEP_SHA_256", Plaintext: payload, BindingContext: binding}, class: corekms.ErrorConfigInvalid},
		{name: "incomplete context", wrap: corekms.WrapRequest{KeyReference: options.KeyARN, Algorithm: options.Algorithm, Plaintext: payload, BindingContext: corekms.BindingContext{"heimdall.context_version": "1"}}, class: corekms.ErrorConfigInvalid},
		{name: "native ciphertext limit", unwrap: corekms.UnwrapRequest{KeyReference: options.KeyARN, Algorithm: options.Algorithm, Ciphertext: make([]byte, MaxAWSCiphertextBytes+1), BindingContext: binding}, class: corekms.ErrorCiphertextInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &mockClient{}
			wrapper, err := newWithClient(options, client)
			if err != nil {
				t.Fatal(err)
			}
			if test.wrap.Plaintext != nil {
				_, err = wrapper.Wrap(context.Background(), test.wrap)
			} else {
				_, err = wrapper.Unwrap(context.Background(), test.unwrap)
			}
			if corekms.Classify(err) != test.class {
				t.Fatalf("class=%q err=%v", corekms.Classify(err), err)
			}
			if client.encryptInput != nil || client.decryptInput != nil {
				t.Fatal("rejected request reached AWS client")
			}
		})
	}
}

func TestAdapterRejectsAWSResponsesOutsideContract(t *testing.T) {
	options := testOptions()
	binding, err := NewBindingContext(corekms.PayloadBinding{InstanceID: "instance-1", SlotID: "slot_primary"}, EncryptionContextPurposePrimary)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{1}, corekms.ProtectedPayloadSize)
	tests := []struct {
		name          string
		encryptOutput *servicekms.EncryptOutput
		decryptOutput *servicekms.DecryptOutput
		operation     corekms.Operation
		class         corekms.ErrorClass
	}{
		{name: "Encrypt wrong Key ARN", operation: corekms.OperationWrap, class: corekms.ErrorCiphertextInvalid, encryptOutput: &servicekms.EncryptOutput{
			CiphertextBlob: []byte("ciphertext"), KeyId: aws.String(strings.Replace(options.KeyARN, "1234", "9999", 1)), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		}},
		{name: "Encrypt wrong algorithm", operation: corekms.OperationWrap, class: corekms.ErrorCiphertextInvalid, encryptOutput: &servicekms.EncryptOutput{
			CiphertextBlob: []byte("ciphertext"), KeyId: aws.String(options.KeyARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecRsaesOaepSha256,
		}},
		{name: "Encrypt oversized ciphertext", operation: corekms.OperationWrap, class: corekms.ErrorCiphertextInvalid, encryptOutput: &servicekms.EncryptOutput{
			CiphertextBlob: make([]byte, MaxAWSCiphertextBytes+1), KeyId: aws.String(options.KeyARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		}},
		{name: "Decrypt wrong Key ARN", operation: corekms.OperationUnwrap, class: corekms.ErrorPayloadInvalid, decryptOutput: &servicekms.DecryptOutput{
			Plaintext: bytes.Clone(payload), KeyId: aws.String(strings.Replace(options.KeyARN, "1234", "9999", 1)), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		}},
		{name: "Decrypt wrong algorithm", operation: corekms.OperationUnwrap, class: corekms.ErrorPayloadInvalid, decryptOutput: &servicekms.DecryptOutput{
			Plaintext: bytes.Clone(payload), KeyId: aws.String(options.KeyARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecRsaesOaepSha256,
		}},
		{name: "Decrypt wrong payload size", operation: corekms.OperationUnwrap, class: corekms.ErrorPayloadInvalid, decryptOutput: &servicekms.DecryptOutput{
			Plaintext: payload[:len(payload)-1], KeyId: aws.String(options.KeyARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		}},
		{name: "Decrypt recipient ciphertext", operation: corekms.OperationUnwrap, class: corekms.ErrorPayloadInvalid, decryptOutput: &servicekms.DecryptOutput{
			Plaintext: bytes.Clone(payload), KeyId: aws.String(options.KeyARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, CiphertextForRecipient: []byte("not permitted"),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &mockClient{encryptOutput: test.encryptOutput, decryptOutput: test.decryptOutput}
			wrapper, err := newWithClient(options, client)
			if err != nil {
				t.Fatal(err)
			}
			if test.operation == corekms.OperationWrap {
				_, err = wrapper.Wrap(context.Background(), corekms.WrapRequest{
					KeyReference: options.KeyARN, Algorithm: options.Algorithm, Plaintext: payload, BindingContext: binding,
				})
			} else {
				_, err = wrapper.Unwrap(context.Background(), corekms.UnwrapRequest{
					KeyReference: options.KeyARN, Algorithm: options.Algorithm, Ciphertext: []byte("ciphertext"), BindingContext: binding,
				})
			}
			if corekms.Classify(err) != test.class {
				t.Fatalf("class=%q err=%v", corekms.Classify(err), err)
			}
			if test.decryptOutput != nil && !allZeroAWS(test.decryptOutput.Plaintext) {
				t.Fatal("rejected AWS plaintext response was not cleared")
			}
		})
	}
}

func TestAWSOptionsRequireExactAccountRegionKeyEndpointAndAlgorithm(t *testing.T) {
	valid := testOptions()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []Options{
		{Region: "ap-southeast-1", Account: valid.Account, KeyARN: strings.Replace(valid.KeyARN, "ap-southeast-1", "us-east-1", 1)},
		{Region: valid.Region, Account: "999999999999", KeyARN: valid.KeyARN},
		{Region: valid.Region, Account: valid.Account, KeyARN: strings.Replace(valid.KeyARN, ":key/", ":alias/", 1)},
		{Region: valid.Region, Account: valid.Account, KeyARN: valid.KeyARN, Endpoint: "http://kms.example.test"},
		{Region: valid.Region, Account: valid.Account, KeyARN: valid.KeyARN, Endpoint: "https://kms.example.test/path"},
		{Region: valid.Region, Account: valid.Account, KeyARN: valid.KeyARN, Algorithm: "RSAES_OAEP_SHA_256"},
	}
	for index, candidate := range tests {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("unsafe options %d accepted: %#v", index, candidate)
		}
	}
}

func TestAWSErrorMappingAndSecretSafeOutput(t *testing.T) {
	tests := []struct {
		code  string
		class corekms.ErrorClass
	}{
		{code: "ThrottlingException", class: corekms.ErrorThrottled},
		{code: "AccessDeniedException", class: corekms.ErrorPermissionDenied},
		{code: "InvalidSignatureException", class: corekms.ErrorPermissionDenied},
		{code: "ExpiredTokenException", class: corekms.ErrorIdentityNotReady},
		{code: "DisabledException", class: corekms.ErrorKeyUnavailable},
		{code: "KMSInvalidStateException", class: corekms.ErrorKeyUnavailable},
		{code: "NotFoundException", class: corekms.ErrorKeyUnavailable},
		{code: "InvalidCiphertextException", class: corekms.ErrorCiphertextInvalid},
		{code: "ValidationException", class: corekms.ErrorConfigInvalid},
		{code: "DependencyTimeoutException", class: corekms.ErrorTransient},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			api := &smithy.GenericAPIError{Code: test.code, Message: "native-secret-response", Fault: smithy.FaultClient}
			header := make(http.Header)
			header.Set("Retry-After", "2")
			native := &awshttp.ResponseError{
				ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 400, Header: header}}, Err: api},
				RequestID:     "aws-request-1",
			}
			err := classifyAWSError(native, corekms.OperationUnwrap)
			if corekms.Classify(err) != test.class {
				t.Fatalf("class=%q err=%v", corekms.Classify(err), err)
			}
			var classified *corekms.Error
			if !errors.As(err, &classified) || classified.ProviderRequestID != "aws-request-1" || classified.RetryAfter != 2*time.Second {
				t.Fatalf("classified=%#v", classified)
			}
			if strings.Contains(err.Error(), "native-secret-response") {
				t.Fatalf("stable error exposed native response: %s", err)
			}
		})
	}
	identity := classifyAWSError(identityError{cause: errors.New("token path secret")}, corekms.OperationUnwrap)
	if corekms.Classify(identity) != corekms.ErrorIdentityNotReady || strings.Contains(identity.Error(), "token path secret") {
		t.Fatalf("identity error=%v", identity)
	}
}

type credentialsProviderFunc func(context.Context) (aws.Credentials, error)

func (f credentialsProviderFunc) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return f(ctx)
}

func TestWorkloadIdentityProviderPreservesSuccessAndClassifiesNotReady(t *testing.T) {
	for _, source := range []string{"WebIdentityCredentials", "CredentialsEndpointProvider", "EC2RoleProvider", "AssumeRoleProvider"} {
		t.Run(source, func(t *testing.T) {
			provider := identityProvider{delegate: credentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: "temporary", SecretAccessKey: "temporary", SessionToken: "temporary", CanExpire: true, Source: source}, nil
			})}
			credentials, err := provider.Retrieve(context.Background())
			if err != nil || credentials.AccessKeyID != "temporary" || !credentials.CanExpire || credentials.Source != source {
				t.Fatalf("credentials=%#v err=%v", credentials, err)
			}
		})
	}
	provider := identityProvider{delegate: credentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, errors.New("projected token is not ready")
	})}
	_, err := provider.Retrieve(context.Background())
	classified := classifyAWSError(err, corekms.OperationUnwrap)
	if corekms.Classify(classified) != corekms.ErrorIdentityNotReady {
		t.Fatalf("class=%q err=%v", corekms.Classify(classified), classified)
	}
	provider = identityProvider{delegate: credentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "long-lived", SecretAccessKey: "must-not-be-used", Source: "EnvConfigCredentials"}, nil
	})}
	_, err = provider.Retrieve(context.Background())
	classified = classifyAWSError(err, corekms.OperationUnwrap)
	if corekms.Classify(classified) != corekms.ErrorIdentityNotReady || strings.Contains(classified.Error(), "EnvConfigCredentials") {
		t.Fatalf("static credential class=%q err=%v", corekms.Classify(classified), classified)
	}
}

func testOptions() Options {
	return Options{
		Region: "ap-southeast-1", Account: "123456789012",
		KeyARN:    "arn:aws:kms:ap-southeast-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		Algorithm: SymmetricDefaultAlgorithm,
	}
}

func mapsEqualForAWSTest(left, right map[string]string) bool {
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

func mapValues(value map[string]string) []string {
	result := make([]string, 0, len(value))
	for _, item := range value {
		result = append(result, item)
	}
	return result
}

func allZeroAWS(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func cloneContextForAWSTest(value corekms.BindingContext) corekms.BindingContext {
	cloned := make(corekms.BindingContext, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}
