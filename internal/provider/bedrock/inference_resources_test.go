package bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/provider"
)

func inferenceResourcesBedrockAdapter(t *testing.T, endpointValue string, profile domain.ProviderProfileID, transport roundTripFunc) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse(endpointValue)
	adapter, err := New(Options{Endpoint: endpoint, CredentialJSON: []byte(testCredential), Client: &http.Client{Transport: transport}, Now: func() time.Time { return time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC) }, ProfileID: profile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func TestTitanImageV2UsesModelFamilySchema(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("png"))
	adapter := inferenceResourcesBedrockAdapter(t, "https://bedrock-runtime.us-east-1.amazonaws.com", domain.ProfileBedrockInvokeTitanImageV2, func(request *http.Request) (*http.Response, error) {
		if request.URL.EscapedPath() != "/model/amazon.titan-image-generator-v2:0/invoke" {
			t.Fatalf("path=%s", request.URL.EscapedPath())
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["taskType"]) != `"TEXT_IMAGE"` {
			t.Fatalf("body=%s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"images":["` + encoded + `"]}`)), Request: request}, nil
	})
	result, err := adapter.GenerateBedrockImage(context.Background(), provider.ImageCall{ProviderModel: "amazon.titan-image-generator-v2:0", Prompt: "owl", Count: 1, Size: "1024x1024"})
	if err != nil || len(result.Data) != 1 || result.Data[0].Base64JSON != encoded {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestAgentRuntimeRerankUsesDistinctSigV4Service(t *testing.T) {
	adapter := inferenceResourcesBedrockAdapter(t, "https://bedrock-agent-runtime.us-east-1.amazonaws.com", domain.ProfileBedrockAgentRerankCohere35, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/rerank" || !strings.Contains(request.Header.Get("Authorization"), "/bedrock-agent-runtime/aws4_request") {
			t.Fatalf("request=%s auth=%s", request.URL, request.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"results":[{"index":1,"relevanceScore":0.9}]}`)), Request: request}, nil
	})
	result, err := adapter.Rerank(context.Background(), provider.RerankCall{ProviderModel: "cohere.rerank-v3-5:0", Query: "q", Documents: []string{"a", "b"}, TopN: 1})
	if err != nil || len(result.Results) != 1 || result.Results[0].Index != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestNovaReelRequiresExplicitS3PrefixBeforeIO(t *testing.T) {
	calls := 0
	adapter := inferenceResourcesBedrockAdapter(t, "https://bedrock-runtime.us-east-1.amazonaws.com", domain.ProfileBedrockAsyncNovaReel, func(request *http.Request) (*http.Response, error) { calls++; return nil, nil })
	_, err := adapter.StartAsyncInvoke(context.Background(), provider.AsyncInvokeCall{ProviderModel: "amazon.nova-reel-v1:0", Prompt: "video", S3OutputURI: "https://bucket/output"})
	if err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
