package bedrock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type credentials struct {
	AccessKeyID     string
	SecretAccessKey []byte
	SessionToken    []byte
	Region          string
}

type signer struct {
	mu           sync.RWMutex
	accessKeyID  string
	secretKey    []byte
	sessionToken []byte
	region       string
	service      string
	authority    string
	now          func() time.Time
}

func newSigner(value credentials) (*signer, error) {
	if !validAccessKey(value.AccessKeyID) {
		return nil, errors.New("AWS access_key_id is invalid")
	}
	if len(value.SecretAccessKey) < 16 || len(value.SecretAccessKey) > 256 {
		return nil, errors.New("AWS secret_access_key is invalid")
	}
	if !validRegion(value.Region) {
		return nil, errors.New("AWS region is invalid")
	}
	return &signer{
		accessKeyID: value.AccessKeyID, secretKey: append([]byte(nil), value.SecretAccessKey...),
		sessionToken: append([]byte(nil), value.SessionToken...), region: value.Region, service: "bedrock", now: time.Now,
	}, nil
}

func (s *signer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.secretKey)
	clear(s.sessionToken)
	s.secretKey = nil
	s.sessionToken = nil
}

func (s *signer) Scheme() domain.CredentialScheme { return domain.CredentialAWSSigV4Explicit }

func (s *signer) Authorize(request *http.Request, payload []byte) error {
	if request == nil || request.URL == nil || request.URL.Host == "" {
		return errors.New("AWS request URL is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.secretKey) == 0 {
		return errors.New("AWS credential authorizer is closed")
	}
	// Compared without the scheme's default port, because that is how the
	// canonical host below is built: a stored endpoint normalized to `:443` and
	// the same endpoint written without it are one audience, not two.
	if !strings.EqualFold(hostWithoutDefaultPort(request.URL.Scheme, request.URL.Host),
		hostWithoutDefaultPort(request.URL.Scheme, s.authority)) {
		return errors.New("AWS credential authorizer audience mismatch")
	}
	now := s.now().UTC()
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	payloadHash := sha256.Sum256(payload)
	payloadHex := hex.EncodeToString(payloadHash[:])
	request.Header.Set("x-amz-date", amzDate)
	request.Header.Set("x-amz-content-sha256", payloadHex)
	if len(s.sessionToken) != 0 {
		request.Header.Set("x-amz-security-token", string(s.sessionToken))
	} else {
		request.Header.Del("x-amz-security-token")
	}
	// The header the request will actually send has to be the authority that was
	// signed, so the port comes off the request as well as off the canonical
	// string. This is what the AWS SDK's own signer does, and skipping it would
	// sign one authority and send another. Only the Host header moves: the URL
	// keeps its port, and the transport still dials it.
	canonicalHost := hostWithoutDefaultPort(request.URL.Scheme, request.URL.Host)
	request.Host = canonicalHost
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	values := map[string]string{
		"host": canonicalHost, "x-amz-content-sha256": payloadHex, "x-amz-date": amzDate,
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		names = append(names, "content-type")
		values["content-type"] = normalizeHeader(contentType)
	}
	// Signed whenever the request carries a body, which is what every AWS SDK
	// signer does. Omitting an optional header is legal — AWS verifies against
	// the SignedHeaders list — but a set that matches the SDK's is a set that can
	// be compared against it, and that comparison is the only independent check
	// this hand-rolled signer has.
	if request.ContentLength > 0 {
		names = append(names, "content-length")
		values["content-length"] = strconv.FormatInt(request.ContentLength, 10)
	}
	if len(s.sessionToken) != 0 {
		names = append(names, "x-amz-security-token")
		values["x-amz-security-token"] = normalizeHeader(string(s.sessionToken))
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		fmt.Fprintf(&canonicalHeaders, "%s:%s\n", name, values[name])
	}
	signedHeaders := strings.Join(names, ";")
	canonicalURI := canonicalSigV4Path(request.URL.EscapedPath())
	// The blank line between the canonical headers and the signed-header list is
	// part of the format, not spacing: SigV4 defines the canonical request as
	// method, URI, query, canonical headers, *an empty line*, signed headers and
	// the payload hash. Without it AWS derives a different string to sign and
	// answers InvalidSignatureException — which is what a real account did to
	// every signed request this adapter made.
	canonicalRequest := request.Method + "\n" + canonicalURI + "\n" + request.URL.RawQuery + "\n" +
		canonicalHeaders.String() + "\n" + signedHeaders + "\n" + payloadHex
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	service := s.service
	if service == "" {
		service = "bedrock"
	}
	scope := date + "/" + s.region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	kDate := hmacSHA256([]byte("AWS4"+string(s.secretKey)), date)
	kRegion := hmacSHA256(kDate, s.region)
	clear(kDate)
	kService := hmacSHA256(kRegion, service)
	clear(kRegion)
	kSigning := hmacSHA256(kService, "aws4_request")
	clear(kService)
	signature := hmacSHA256(kSigning, stringToSign)
	clear(kSigning)
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.accessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+hex.EncodeToString(signature))
	clear(signature)
	return nil
}

// canonicalSigV4Path is the request path as SigV4 canonicalizes it for every
// service except S3: the path that goes on the wire, URI-encoded a second time.
//
// A Bedrock model id carries a colon — `anthropic.claude-sonnet-4-5-...-v1:0`,
// and an inference-profile ARN carries several — so this is not a corner the
// adapter can skip. Signing the once-encoded path produced a signature AWS
// could not reproduce, and the account answered InvalidSignatureException.
//
// Unreserved characters (RFC 3986) and the separator pass through; everything
// else becomes uppercase percent-hex. Kept as code rather than as a dependency:
// the AWS SDK's escaper would bring the SDK into the request path, and this
// adapter signs by hand on purpose. The equivalence with the SDK's escaper is
// asserted in signer_vector_test.go.
func canonicalSigV4Path(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	var canonical strings.Builder
	for index := 0; index < len(escapedPath); index++ {
		char := escapedPath[index]
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9',
			char == '-', char == '.', char == '_', char == '~', char == '/':
			canonical.WriteByte(char)
		default:
			fmt.Fprintf(&canonical, "%%%02X", char)
		}
	}
	return canonical.String()
}

// hostWithoutDefaultPort is the authority as SigV4 canonicalizes it: a port that
// is the scheme's default is not part of the host.
//
// Halro normalizes every stored Provider endpoint to an explicit port, so the
// adapter signs `https://host:443/...` where an AWS SDK would sign
// `https://host/...`. Signing the port produced a signature the account could
// not reproduce — InvalidSignatureException on every request — while the
// port-less form matches the SDK's byte for byte.
func hostWithoutDefaultPort(scheme, host string) string {
	if host == "" {
		return host
	}
	parsed := url.URL{Scheme: scheme, Host: host}
	port := parsed.Port()
	if port == "" {
		return host
	}
	if !(strings.EqualFold(scheme, "https") && port == "443" ||
		strings.EqualFold(scheme, "http") && port == "80") {
		return host
	}
	name := parsed.Hostname()
	if strings.Contains(name, ":") {
		return "[" + name + "]"
	}
	return name
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func normalizeHeader(value string) string { return strings.Join(strings.Fields(value), " ") }

func validAccessKey(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func validRegion(value string) bool {
	if len(value) < 3 || len(value) > 64 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return false
	}
	return strings.Contains(value, "-")
}
