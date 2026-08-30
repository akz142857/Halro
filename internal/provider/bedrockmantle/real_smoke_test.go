package bedrockmantle

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	anthropicprovider "github.com/akz142857/Halro/internal/provider/anthropic"
	openaiprovider "github.com/akz142857/Halro/internal/provider/openai"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// TestRealProviderSmoke contacts a real Amazon Bedrock Mantle account and costs
// money. It is inert unless every variable below is set deliberately.
//
//	HALRO_REAL_PROVIDER_SMOKE=1
//	HALRO_SMOKE_PROFILE=bedrock_mantle
//	HALRO_SMOKE_BASE_URL=https://bedrock-mantle.<region>.api.aws
//	HALRO_SMOKE_API_KEY=<Bedrock API key>
//	HALRO_SMOKE_MODEL=<exact upstream model id>
//	HALRO_SMOKE_MANTLE_PROFILE=chat|openai-chat|responses|openai-responses|messages
//
// The chat and responses values name the default /v1 route; the openai-
// prefixed ones name the second /openai/v1 route. A model reaches exactly one
// of the two, so pairing HALRO_SMOKE_BEDROCK_MODEL with the wrong value here
// is refused upstream rather than silently served.
//
//	HALRO_SMOKE_BEDROCK_PROJECT_ID=<proj_...>   (optional; empty means default)
//
// One run proves one cell of the matrix in docs/verification/provider-real-matrix.md:
// commit × region × profile × exact model × authentication × project mode. It
// says nothing about the other two wire profiles, another model, or another
// region, and the runner records it that way.
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "bedrock_mantle" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=bedrock_mantle")
	}
	endpoint, err := url.Parse(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if err != nil {
		t.Fatal("HALRO_SMOKE_BASE_URL must be a valid URL")
	}
	// The same endpoint rule the Admin API applies, so a smoke cannot pass
	// against a host the product would refuse to store.
	if err := ValidateEndpoint(endpoint); err != nil {
		t.Fatalf("HALRO_SMOKE_BASE_URL is not an accepted Mantle endpoint: %v", err)
	}
	apiKey := os.Getenv("HALRO_SMOKE_API_KEY")
	model := os.Getenv("HALRO_SMOKE_MODEL")
	if apiKey == "" || model == "" {
		t.Fatal("HALRO_SMOKE_API_KEY and HALRO_SMOKE_MODEL are required")
	}
	projectID := domain.NormalizeBedrockProjectID(os.Getenv("HALRO_SMOKE_BEDROCK_PROJECT_ID"))
	if err := domain.ValidateBedrockProjectID(projectID); err != nil {
		t.Fatalf("HALRO_SMOKE_BEDROCK_PROJECT_ID: %v", err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	adapter := newSmokeAdapter(t, os.Getenv("HALRO_SMOKE_MANTLE_PROFILE"), endpoint, apiKey, projectID, client)
	defer adapter.Close()
	smokeChatBoth(ctx, t, adapter, model)
}

// newSmokeAdapter builds the adapter one wire profile runs on, exactly as the
// composition root does. It is a helper rather than an inline switch because a
// second test needs the same five constructions, and two copies of this is how
// one of them ends up wired differently from the product.
func newSmokeAdapter(t *testing.T, wire string, endpoint *url.URL, apiKey, projectID string, client *http.Client) provider.Adapter {
	t.Helper()
	switch wire {
	case "chat", "openai-chat":
		chatProfile := domain.ProfileBedrockMantleChat
		if wire == "openai-chat" {
			chatProfile = domain.ProfileBedrockMantleOpenAIChat
		}
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte(apiKey), "api-key", "x-api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := openaiprovider.NewWithOptions(openaiprovider.Options{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			ProviderType:        string(domain.ProviderBedrock),
			CredentialScheme:    domain.CredentialBedrockAPIKey,
			BedrockProjectID:    projectID,
			Capabilities:        domainCapabilities(chatProfile),
			OperationPathPrefix: smokeMantlePrefix(wire),
		})
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	case "responses", "openai-responses":
		responsesProfile := domain.ProfileBedrockMantleResponses
		if wire == "openai-responses" {
			responsesProfile = domain.ProfileBedrockMantleOpenAIResponses
		}
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte(apiKey), "api-key", "x-api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := NewResponses(ResponsesOptions{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			BedrockProjectID:    projectID,
			Capabilities:        domainCapabilities(responsesProfile),
			OperationPathPrefix: smokeMantlePrefix(wire),
		})
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	case "messages":
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte(apiKey), "Authorization", "api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := anthropicprovider.New(anthropicprovider.Options{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			ProviderType:     string(domain.ProviderBedrock),
			CredentialScheme: domain.CredentialBedrockAPIKey,
			MessagesPath:     "anthropic/v1/messages",
			BedrockProjectID: projectID,
			Capabilities:     domainCapabilities(domain.ProfileBedrockMantleAnthropicMessages),
		})
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	default:
		t.Fatalf("HALRO_SMOKE_MANTLE_PROFILE must be chat, openai-chat, responses, openai-responses, or messages (got %q)", wire)
		return nil
	}
}

// TestRealProviderSendsTheBedrockProject proves that a project named on a
// connection leaves this process. It contacts a real account and costs money;
// it is inert unless the same variables the smoke above needs are set.
//
// It exists because the smoke above cannot prove this and never could. That one
// asserts a request succeeds, and a project header the service ignores succeeds
// too — which is exactly how Halro shipped `anthropic-workspace`, a name no
// service reads, for as long as it did. A green smoke was consistent with the
// defect.
//
// The discriminator is a project id that does not exist. A service that reads
// the header has to reject it; a service that does not read it cannot. So the
// test is two calls and their disagreement is the evidence:
//
//   - with no project, the account default, the call must succeed. Without this
//     half a failure below could be a bad key, a bad model, or a bad endpoint.
//   - with `proj_` and twenty z's, the call must fail. If it succeeds, the
//     header did not reach the service, or this route does not validate it.
//
// Measured on 2026-08-29 against `/anthropic/v1/messages`, where the same pair
// answered 404 and 200 through curl. The OpenAI-shaped routes carry the same
// resource as `OpenAI-Project` and are **not** measured: a failure there is a
// finding to record in docs/verification/provider-real-matrix.md, not a broken
// test.
//
// Cost is one inference. The refused call is refused before the model runs.
func TestRealProviderSendsTheBedrockProject(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "bedrock_mantle" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=bedrock_mantle")
	}
	endpoint, err := url.Parse(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if err != nil {
		t.Fatal("HALRO_SMOKE_BASE_URL must be a valid URL")
	}
	if err := ValidateEndpoint(endpoint); err != nil {
		t.Fatalf("HALRO_SMOKE_BASE_URL is not an accepted Mantle endpoint: %v", err)
	}
	apiKey := os.Getenv("HALRO_SMOKE_API_KEY")
	model := os.Getenv("HALRO_SMOKE_MODEL")
	if apiKey == "" || model == "" {
		t.Fatal("HALRO_SMOKE_API_KEY and HALRO_SMOKE_MODEL are required")
	}
	wire := os.Getenv("HALRO_SMOKE_MANTLE_PROFILE")
	client := &http.Client{Timeout: 60 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The id is refused by the service, not by Halro: it has to be a well formed
	// project so it reaches the wire, which is what ValidateBedrockProjectID
	// accepts and what this asserts before spending anything.
	const absent = "proj_zzzzzzzzzzzzzzzzzzzz"
	if err := domain.ValidateBedrockProjectID(absent); err != nil {
		t.Fatalf("the control project id is not well formed, so it would never reach the service: %v", err)
	}

	baseline := newSmokeAdapter(t, wire, endpoint, apiKey, "", client)
	defer baseline.Close()
	if err := smokeChatOnce(ctx, baseline, model); err != nil {
		t.Fatalf("the default project could not serve a request, so nothing below would mean anything: %v", err)
	}

	named := newSmokeAdapter(t, wire, endpoint, apiKey, absent, client)
	defer named.Close()
	if err := smokeChatOnce(ctx, named, model); err == nil {
		t.Fatalf("a project that does not exist was served, so the %s header did not reach the service or this route does not validate it — record which in docs/verification/provider-real-matrix.md", wire)
	} else {
		// Logged, not asserted on: the wording is the service's and pinning it
		// would make this test fail on a message change rather than on a
		// behaviour change.
		t.Logf("the absent project was refused, as it must be: %v", err)
	}
}

// smokeChatOnce is one non-streaming call, returning the error instead of
// failing, because the test above needs a call whose failure is the assertion.
func smokeChatOnce(ctx context.Context, adapter provider.Adapter, model string) error {
	chat, ok := adapter.(interface {
		Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error)
	})
	if !ok {
		return errors.New("adapter cannot serve chat")
	}
	request := openaiapi.ChatCompletionRequest{
		Messages:            []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}},
		MaxCompletionTokens: smokePointer(int64(8)),
	}
	_, err := chat.Chat(ctx, provider.ChatCall{RequestID: "mantle-project-header", ProviderModel: model, Request: request})
	return err
}

// smokeChatBoth exercises the two shapes every Mantle profile must serve. It
// asserts structure only: no response text is printed, because the runner
// captures this process's output into an evidence file.
func smokeChatBoth(ctx context.Context, t *testing.T, adapter provider.Adapter, model string) {
	t.Helper()
	chat, ok := adapter.(interface {
		Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error)
	})
	if !ok {
		t.Fatal("adapter cannot serve chat")
	}
	request := openaiapi.ChatCompletionRequest{
		Messages:            []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}},
		MaxCompletionTokens: smokePointer(int64(8)),
	}
	response, err := chat.Chat(ctx, provider.ChatCall{RequestID: "mantle-real-smoke", ProviderModel: model, Request: request})
	if err != nil {
		t.Fatalf("non-stream chat: %v", err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message == nil {
		t.Fatal("non-stream chat returned no assistant choice")
	}
	if response.Usage == nil || response.Usage.TotalTokens == 0 {
		t.Fatal("non-stream chat reported no usage, so this run cannot be accounted")
	}

	streamer, ok := adapter.(interface {
		ChatStream(context.Context, provider.ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error)
	})
	if !ok {
		t.Fatal("adapter cannot serve streaming chat")
	}
	events := 0
	usage, err := streamer.ChatStream(ctx, provider.ChatCall{RequestID: "mantle-real-smoke-stream", ProviderModel: model, Request: request}, func(semantic.Event) error {
		events++
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if events == 0 {
		t.Fatal("stream chat produced no events")
	}
	if usage == nil || usage.TotalTokens == 0 {
		t.Fatal("stream chat reported no usage, so this run cannot be accounted")
	}
}

// domainCapabilities gives the adapter exactly the profile's ceiling, so a
// smoke cannot accidentally exercise a capability the product refuses to
// declare.
func domainCapabilities(profileID domain.ProviderProfileID) provider.Capabilities {
	declared := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, profileID)
	return provider.Capabilities{
		Chat: declared.Chat, Streaming: declared.Streaming, Embeddings: declared.Embeddings,
		Tools: declared.Tools, Vision: declared.Vision,
		JSONObject: declared.JSONObject, StructuredOutputs: declared.StructuredOutputs,
		DeveloperRole: declared.DeveloperRole, Reasoning: declared.Reasoning,
		StreamUsage: declared.StreamUsage,
		Moderations: declared.Moderations, Images: declared.Images,
		Transcriptions: declared.Transcriptions, Speech: declared.Speech,
		Files: declared.Files, Batches: declared.Batches, Rerank: declared.Rerank,
		AsyncGenerate:    declared.AsyncGenerate,
		MaxContextTokens: declared.MaxContextTokens, MaxOutputTokens: declared.MaxOutputTokens,
	}
}

func smokePointer[T any](value T) *T { return &value }

// smokeMantlePrefix maps the selector to the route the profile addresses. The
// empty string is the default /v1 join the adapters already do.
func smokeMantlePrefix(wire string) string {
	if strings.HasPrefix(wire, "openai-") {
		return "openai/v1"
	}
	return ""
}
