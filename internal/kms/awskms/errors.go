package awskms

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	corekms "github.com/akz142857/Heimdall/internal/kms"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

const maxRetryAfter = 30 * time.Second

func classifyAWSError(err error, operation corekms.Operation) error {
	class := corekms.ErrorTransient
	var identity identityError
	var api smithy.APIError
	switch {
	case errors.As(err, &identity):
		class = corekms.ErrorIdentityNotReady
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		class = corekms.ErrorTransient
	case errors.As(err, &api):
		class = classifyAPIError(api.ErrorCode())
	default:
		var response *awshttp.ResponseError
		if errors.As(err, &response) && response.Response != nil {
			class = classifyHTTPStatus(response.Response.StatusCode)
		}
	}
	retryAfter, requestID := responseMetadata(err)
	classified := corekms.NewError(class, Provider, operation, retryAfter, err)
	classified.ProviderRequestID = requestID
	return classified
}

func classifyAPIError(code string) corekms.ErrorClass {
	switch code {
	case "ThrottlingException", "LimitExceededException":
		return corekms.ErrorThrottled
	case "DependencyTimeoutException", "KeyUnavailableException":
		return corekms.ErrorTransient
	case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "UnrecognizedClientException":
		return corekms.ErrorIdentityNotReady
	case "AccessDeniedException", "InvalidGrantTokenException", "ExpiredImportTokenException",
		"IncompleteSignature", "InvalidSignatureException", "MissingAuthenticationToken", "SignatureDoesNotMatch":
		return corekms.ErrorPermissionDenied
	case "DisabledException", "KMSInvalidStateException", "NotFoundException":
		return corekms.ErrorKeyUnavailable
	case "InvalidCiphertextException", "IncorrectKeyException":
		return corekms.ErrorCiphertextInvalid
	case "InvalidArnException", "InvalidKeyUsageException", "ValidationException", "UnsupportedOperationException":
		return corekms.ErrorConfigInvalid
	default:
		return corekms.ErrorTransient
	}
}

func classifyHTTPStatus(status int) corekms.ErrorClass {
	switch {
	case status == http.StatusTooManyRequests:
		return corekms.ErrorThrottled
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return corekms.ErrorPermissionDenied
	case status == http.StatusBadRequest:
		return corekms.ErrorConfigInvalid
	case status == http.StatusRequestTimeout || status >= 500:
		return corekms.ErrorTransient
	default:
		return corekms.ErrorTransient
	}
}

func responseMetadata(err error) (time.Duration, string) {
	var response *awshttp.ResponseError
	if !errors.As(err, &response) || response == nil {
		return 0, ""
	}
	retryAfter := time.Duration(0)
	if response.Response != nil && response.Response.Header != nil {
		raw := strings.TrimSpace(response.Response.Header.Get("Retry-After"))
		if seconds, parseErr := strconv.Atoi(raw); parseErr == nil && seconds > 0 {
			retryAfter = time.Duration(seconds) * time.Second
			if retryAfter > maxRetryAfter {
				retryAfter = maxRetryAfter
			}
		}
	}
	requestID := response.ServiceRequestID()
	if len(requestID) > corekms.MaxProviderRequestIDBytes {
		requestID = ""
	}
	return retryAfter, requestID
}
