package bedrock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
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
	if !strings.EqualFold(request.URL.Host, s.authority) {
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
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	values := map[string]string{
		"host": request.URL.Host, "x-amz-content-sha256": payloadHex, "x-amz-date": amzDate,
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		names = append(names, "content-type")
		values["content-type"] = normalizeHeader(contentType)
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
	canonicalURI := request.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalRequest := request.Method + "\n" + canonicalURI + "\n" + request.URL.RawQuery + "\n" +
		canonicalHeaders.String() + signedHeaders + "\n" + payloadHex
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
