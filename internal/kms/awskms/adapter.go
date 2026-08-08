package awskms

import (
	"bytes"
	"context"
	"errors"

	corekms "github.com/akz142857/Halro/internal/kms"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	servicekms "github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

type client interface {
	Encrypt(context.Context, *servicekms.EncryptInput, ...func(*servicekms.Options)) (*servicekms.EncryptOutput, error)
	Decrypt(context.Context, *servicekms.DecryptInput, ...func(*servicekms.Options)) (*servicekms.DecryptOutput, error)
}

type Wrapper struct {
	options Options
	client  client
}

func New(ctx context.Context, options Options) (*Wrapper, error) {
	options = options.Normalized()
	if err := options.Validate(); err != nil {
		return nil, corekms.NewError(corekms.ErrorConfigInvalid, Provider, corekms.OperationUnwrap, 0, err)
	}
	config, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(options.Region),
		awsconfig.WithRetryMaxAttempts(1),
	)
	if err != nil {
		return nil, corekms.NewError(corekms.ErrorIdentityNotReady, Provider, corekms.OperationUnwrap, 0, err)
	}
	if config.Credentials != nil {
		config.Credentials = aws.NewCredentialsCache(identityProvider{delegate: config.Credentials})
	}
	awsClient := servicekms.NewFromConfig(config, func(value *servicekms.Options) {
		if options.Endpoint != "" {
			value.BaseEndpoint = aws.String(options.Endpoint)
		}
	})
	return newWithClient(options, awsClient)
}

func newWithClient(options Options, awsClient client) (*Wrapper, error) {
	options = options.Normalized()
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if awsClient == nil {
		return nil, errors.New("AWS KMS client is required")
	}
	return &Wrapper{options: options, client: awsClient}, nil
}

func (w *Wrapper) Provider() string { return Provider }

func (w *Wrapper) Wrap(ctx context.Context, request corekms.WrapRequest) (corekms.WrapResult, error) {
	if err := request.Validate(); err != nil {
		return corekms.WrapResult{}, corekms.NewError(corekms.ErrorConfigInvalid, Provider, corekms.OperationWrap, 0, err)
	}
	if err := w.validateRequestPolicy(request.KeyReference, request.Algorithm, request.BindingContext); err != nil {
		return corekms.WrapResult{}, corekms.NewError(corekms.ErrorConfigInvalid, Provider, corekms.OperationWrap, 0, err)
	}
	plaintext := bytes.Clone(request.Plaintext)
	defer clear(plaintext)
	output, err := w.client.Encrypt(ctx, &servicekms.EncryptInput{
		KeyId: aws.String(w.options.KeyARN), Plaintext: plaintext,
		EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext:   cloneStringMap(request.BindingContext),
	})
	if err != nil {
		return corekms.WrapResult{}, classifyAWSError(err, corekms.OperationWrap)
	}
	if output == nil || len(output.CiphertextBlob) == 0 || len(output.CiphertextBlob) > MaxAWSCiphertextBytes ||
		aws.ToString(output.KeyId) != w.options.KeyARN || output.EncryptionAlgorithm != types.EncryptionAlgorithmSpecSymmetricDefault {
		return corekms.WrapResult{}, corekms.NewError(corekms.ErrorCiphertextInvalid, Provider, corekms.OperationWrap, 0, errors.New("AWS KMS Encrypt response violated the allowlist contract"))
	}
	requestID, _ := awsmiddleware.GetRequestIDMetadata(output.ResultMetadata)
	result := corekms.WrapResult{Ciphertext: bytes.Clone(output.CiphertextBlob), ProviderRequestID: requestID}
	if err := result.Validate(); err != nil {
		return corekms.WrapResult{}, corekms.NewError(corekms.ErrorCiphertextInvalid, Provider, corekms.OperationWrap, 0, err)
	}
	return result, nil
}

func (w *Wrapper) Unwrap(ctx context.Context, request corekms.UnwrapRequest) (corekms.UnwrapResult, error) {
	if err := request.Validate(); err != nil {
		return corekms.UnwrapResult{}, corekms.NewError(corekms.ErrorConfigInvalid, Provider, corekms.OperationUnwrap, 0, err)
	}
	if len(request.Ciphertext) > MaxAWSCiphertextBytes {
		return corekms.UnwrapResult{}, corekms.NewError(corekms.ErrorCiphertextInvalid, Provider, corekms.OperationUnwrap, 0, errors.New("AWS KMS ciphertext exceeds native limit"))
	}
	if err := w.validateRequestPolicy(request.KeyReference, request.Algorithm, request.BindingContext); err != nil {
		return corekms.UnwrapResult{}, corekms.NewError(corekms.ErrorConfigInvalid, Provider, corekms.OperationUnwrap, 0, err)
	}
	output, err := w.client.Decrypt(ctx, &servicekms.DecryptInput{
		KeyId: aws.String(w.options.KeyARN), CiphertextBlob: bytes.Clone(request.Ciphertext),
		EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext:   cloneStringMap(request.BindingContext),
	})
	if err != nil {
		return corekms.UnwrapResult{}, classifyAWSError(err, corekms.OperationUnwrap)
	}
	if output == nil {
		return corekms.UnwrapResult{}, corekms.NewError(corekms.ErrorPayloadInvalid, Provider, corekms.OperationUnwrap, 0, errors.New("AWS KMS Decrypt response is empty"))
	}
	defer clear(output.Plaintext)
	if len(output.Plaintext) != corekms.ProtectedPayloadSize || aws.ToString(output.KeyId) != w.options.KeyARN ||
		output.EncryptionAlgorithm != types.EncryptionAlgorithmSpecSymmetricDefault || len(output.CiphertextForRecipient) != 0 {
		return corekms.UnwrapResult{}, corekms.NewError(corekms.ErrorPayloadInvalid, Provider, corekms.OperationUnwrap, 0, errors.New("AWS KMS Decrypt response violated the protected-payload contract"))
	}
	requestID, _ := awsmiddleware.GetRequestIDMetadata(output.ResultMetadata)
	result := corekms.UnwrapResult{Plaintext: bytes.Clone(output.Plaintext), ProviderRequestID: requestID}
	if err := result.Validate(); err != nil {
		clear(result.Plaintext)
		return corekms.UnwrapResult{}, corekms.NewError(corekms.ErrorPayloadInvalid, Provider, corekms.OperationUnwrap, 0, err)
	}
	return result, nil
}

func (w *Wrapper) validateRequestPolicy(keyReference, algorithm string, binding corekms.BindingContext) error {
	if keyReference != w.options.KeyARN || algorithm != w.options.Algorithm {
		return errors.New("AWS KMS request is outside the configured key/algorithm allowlist")
	}
	return validateBindingContext(binding)
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

type identityError struct{ cause error }

func (e identityError) Error() string { return "AWS Workload Identity is not ready" }
func (e identityError) Unwrap() error { return e.cause }

type identityProvider struct{ delegate aws.CredentialsProvider }

func (p identityProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	credentials, err := p.delegate.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, identityError{cause: err}
	}
	if !workloadIdentitySource(credentials.Source) {
		clearCredentialStrings(&credentials)
		return aws.Credentials{}, identityError{cause: errors.New("AWS credential source is not an approved Workload Identity provider")}
	}
	return credentials, nil
}

func workloadIdentitySource(source string) bool {
	switch source {
	case "WebIdentityCredentials", // EKS IRSA and other OIDC federation.
		"CredentialsEndpointProvider", // ECS task role and EKS Pod Identity.
		"EC2RoleProvider":             // EC2 instance profile.
		return true
	default:
		return false
	}
}

func clearCredentialStrings(credentials *aws.Credentials) {
	if credentials == nil {
		return
	}
	credentials.AccessKeyID = ""
	credentials.SecretAccessKey = ""
	credentials.SessionToken = ""
}
