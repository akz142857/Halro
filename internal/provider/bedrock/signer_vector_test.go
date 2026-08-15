package bedrock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
)

// The signature this adapter produces, checked against an independent
// implementation rather than against itself.
//
// Halro signs Bedrock by hand — the SDK signer would drag in credential
// resolution the gateway must not have — and the hand-rolled canonical request
// was missing the empty line SigV4 requires between the canonical headers and
// the signed-header list. Every existing test asserted the *shape* of the
// Authorization header (its prefix, that the secret never appears in it), which
// a wrong signature satisfies perfectly. A real account answered
// InvalidSignatureException to every signed request, and nothing in the tree
// disagreed.
//
// The SDK is already a dependency here for KMS, so its signer is available as an
// oracle in a test without becoming part of the request path.
func TestSignatureMatchesAnIndependentSigV4Implementation(t *testing.T) {
	const (
		accessKeyID  = "AKIDEXAMPLE12345678"
		secretKey    = "test-secret-access-key-value"
		sessionToken = "session-token"
		region       = "us-east-1"
		service      = "bedrock"
	)
	at := time.Date(2026, 7, 31, 12, 34, 56, 0, time.UTC)

	for _, test := range []struct {
		name    string
		method  string
		target  string
		payload string
		headers map[string]string
	}{
		{
			// The connection probe: the operation's own method, carrying a body
			// the operation must reject.
			name: "post with a json body", method: http.MethodPost,
			target:  "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-test-v1:0/converse",
			payload: "{}", headers: map[string]string{"Content-Type": "application/json"},
		},
		{
			// A model id whose characters have to survive path escaping
			// identically on both sides, or the canonical URI differs.
			name: "post to an escaped model path", method: http.MethodPost,
			target:  "https://bedrock-runtime.us-east-1.amazonaws.com/model/arn:aws:bedrock:us-east-1:1234:inference-profile/us.anthropic.claude-test-v1:0/converse",
			payload: `{"messages":[]}`, headers: map[string]string{"Content-Type": "application/json"},
		},
		{
			// The endpoint Halro stores is normalized with an explicit port, so
			// this is the URL the adapter actually signs in production.
			name: "post to an endpoint carrying the default port", method: http.MethodPost,
			target:  "https://bedrock-runtime.us-east-1.amazonaws.com:443/model/anthropic.claude-test-v1:0/converse",
			payload: "{}", headers: map[string]string{"Content-Type": "application/json"},
		},
		{
			name: "get with no body", method: http.MethodGet,
			target: "https://bedrock-runtime.us-east-1.amazonaws.com/foundation-models",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint, err := url.Parse(test.target)
			if err != nil {
				t.Fatal(err)
			}
			signed, err := newSigner(credentials{
				AccessKeyID: accessKeyID, SecretAccessKey: []byte(secretKey),
				SessionToken: []byte(sessionToken), Region: region,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer signed.Close()
			signed.now = func() time.Time { return at }
			signed.authority = endpoint.Host

			ours := buildRequest(t, test.method, test.target, test.payload, test.headers)
			if err := signed.Authorize(ours, []byte(test.payload)); err != nil {
				t.Fatal(err)
			}

			// The oracle signs the same request, with the same headers the
			// canonical request is built from present before signing.
			theirs := buildRequest(t, test.method, test.target, test.payload, test.headers)
			for _, name := range []string{"x-amz-date", "x-amz-content-sha256", "x-amz-security-token"} {
				if value := ours.Header.Get(name); value != "" {
					theirs.Header.Set(name, value)
				}
			}
			sum := sha256.Sum256([]byte(test.payload))
			credentials := awscreds.NewStaticCredentialsProvider(accessKeyID, secretKey, sessionToken)
			resolved, err := credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := awsv4.NewSigner().SignHTTP(
				context.Background(), resolved, theirs, hex.EncodeToString(sum[:]), service, region, at,
			); err != nil {
				t.Fatal(err)
			}

			want, got := theirs.Header.Get("Authorization"), ours.Header.Get("Authorization")
			if want == "" {
				t.Fatal("the oracle produced no Authorization header")
			}
			if got != want {
				t.Fatalf("signature disagrees with an independent SigV4 implementation:\n ours: %s\ntheirs: %s", got, want)
			}
			if strings.Contains(got, secretKey) {
				t.Fatal("the Authorization header carried the secret key")
			}
			// The authority that goes on the wire has to be the one that was
			// signed; the oracle rewrites the request's Host for the same reason.
			if ours.Host != theirs.Host {
				t.Fatalf("sent host=%q, signed host=%q", ours.Host, theirs.Host)
			}
		})
	}
}

func buildRequest(t *testing.T, method, target, payload string, headers map[string]string) *http.Request {
	t.Helper()
	var body *strings.Reader
	if payload != "" {
		body = strings.NewReader(payload)
	}
	var request *http.Request
	var err error
	if body != nil {
		request, err = http.NewRequest(method, target, body)
	} else {
		request, err = http.NewRequest(method, target, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return request
}
