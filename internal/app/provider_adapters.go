package app

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	anthropicprovider "github.com/akz142857/Halro/internal/provider/anthropic"
	bedrockprovider "github.com/akz142857/Halro/internal/provider/bedrock"
	bedrockmantleprovider "github.com/akz142857/Halro/internal/provider/bedrockmantle"
	geminiprovider "github.com/akz142857/Halro/internal/provider/gemini"
	openaiprovider "github.com/akz142857/Halro/internal/provider/openai"
)

// How a stored connection becomes an adapter, as a table rather than a switch.
//
// This is one entry per profile because that is the granularity the decision
// actually has: two profiles on one provider type can want different credential
// headers and different adapters, which the Bedrock and MiniMax rows both do.
// Expressed as a switch it was a type switch containing two nested profile
// switches, and the mapping could not be read in one place or enumerated by
// anything.
//
// What this does not buy is compile-time coverage, and it is worth being plain
// about that rather than implying it. Go does not check a map literal for
// completeness any more than it checks a switch, so a platform added to the
// profile table and forgotten here still compiles. The guard is
// TestEveryReachableProfileBuildsAnAdapter, which walks the domain table and
// builds every non-withheld profile; the table below is what lets it report a
// missing row instead of an unreachable default branch.
//
// Reaching compile-time coverage would mean putting the constructor in the
// profile table row itself, and the table lives in domain, which cannot import
// a platform package. That is the same layering that keeps the capability
// ceiling where ProviderProfileBinding.Validate can reach it.

// adapterBuildContext is everything a profile's construction is given.
type adapterBuildContext struct {
	Instance  domain.ProviderInstance
	Binding   domain.ProviderProfileBinding
	Endpoint  *url.URL
	Plaintext []byte
	Client    *http.Client
}

// adapterBuilder is one profile's construction, in the order it happens:
// validate the endpoint this profile will address, turn the stored secret into
// a request header, then build the adapter.
//
// The three stages are separate because they fail differently and the ordering
// is load-bearing: Bedrock Mantle refuses an endpoint that is not one of its
// own hosts, and it does so before a credential is ever unwrapped.
type adapterBuilder struct {
	// validateEndpoint is optional. Only a profile bound to specific hosts has
	// one, and it runs before the credential is touched.
	validateEndpoint func(*url.URL) error
	authorize        func(adapterBuildContext) (provider.Authorizer, error)
	build            func(adapterBuildContext, provider.Authorizer) (provider.Adapter, error)
}

// staticHeader is the credential scheme most profiles use: the secret becomes
// one header, and any other header that could carry a credential is cleared so
// two schemes cannot both be sent.
func staticHeader(name, prefix string, conflicting ...string) func(adapterBuildContext) (provider.Authorizer, error) {
	return func(ctx adapterBuildContext) (provider.Authorizer, error) {
		return provider.NewStaticHeaderAuthorizer(ctx.Binding.CredentialScheme, name, prefix, ctx.Plaintext, conflicting...)
	}
}

// bedrockSigV4 is the one credential that is not a header at rest: it is signed
// per request against the endpoint's own region.
func bedrockSigV4(ctx adapterBuildContext) (provider.Authorizer, error) {
	return bedrockprovider.NewAuthorizer(ctx.Endpoint, ctx.Plaintext, nil)
}

var adapterBuilders = map[domain.ProviderProfileID]adapterBuilder{
	// OpenAI, its compatibility servers, and DeepSeek share one adapter and one
	// credential shape. The provider type is carried through because the adapter
	// reports it and, for DeepSeek, switches to a smaller request body.
	domain.ProfileOpenAIChatEmbeddings: {
		authorize: staticHeader("Authorization", "Bearer ", "api-key"),
		build:     openAICompatibleAdapter(false),
	},
	// The same account and credential against a different endpoint. Which
	// surface a connection addresses is the profile's to say and no request's.
	domain.ProfileOpenAIResponses: {
		authorize: staticHeader("Authorization", "Bearer ", "api-key"),
		build:     openAICompatibleAdapter(true),
	},
	domain.ProfileOpenAIMediaResources: {
		authorize: staticHeader("Authorization", "Bearer ", "api-key"),
		build:     openAICompatibleAdapter(false),
	},
	domain.ProfileDeepSeekChat: {
		authorize: staticHeader("Authorization", "Bearer ", "api-key"),
		build:     openAICompatibleAdapter(false),
	},
	domain.ProfileOpenAICompatible: {
		authorize: staticHeader("Authorization", "Bearer ", "api-key"),
		build:     openAICompatibleAdapter(false),
	},
	domain.ProfileAnthropicMessages: {
		authorize: staticHeader("x-api-key", "", "Authorization"),
		build: func(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
			return anthropicprovider.New(anthropicprovider.Options{
				Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
				Capabilities: ctx.Binding.Capabilities, ProfileID: ctx.Binding.ProfileID,
			})
		},
	},
	domain.ProfileAzureChatEmbeddings: {
		authorize: staticHeader("api-key", "", "Authorization"),
		build: func(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
			return openaiprovider.NewWithOptions(openaiprovider.Options{
				Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
				ProviderType: string(ctx.Instance.Type), APIVersion: ctx.Instance.APIVersion,
				Azure: true, Capabilities: ctx.Binding.Capabilities,
			})
		},
	},
	domain.ProfileGeminiText: {
		authorize: staticHeader("x-goog-api-key", "", "Authorization"),
		build: func(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
			return geminiprovider.New(geminiprovider.Options{Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client})
		},
	},

	// MiniMax: one host, one bearer key, three wire shapes. The key is accepted
	// as a bearer token everywhere and additionally as x-api-key on the Anthropic
	// route, with Authorization taking precedence when both are present, so the
	// bearer form is the one that works on every profile.
	domain.ProfileMiniMaxAnthropicMessages: {
		authorize: staticHeader("Authorization", "Bearer ", "x-api-key"),
		build: func(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
			return anthropicprovider.New(anthropicprovider.Options{
				Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
				Capabilities: ctx.Binding.Capabilities,
				ProviderType: string(domain.ProviderMiniMax), CredentialScheme: ctx.Binding.CredentialScheme,
				MessagesPath: "anthropic/v1/messages", ProfileID: ctx.Binding.ProfileID,
				// MiniMax keeps its model list on the OpenAI route of the same host,
				// reachable with this same key, and answers it in OpenAI's shape.
				// Measured against a real account on 2026-09-01: object=list, eight
				// entries of {id, object, created, owned_by}, every one a chat model.
				// So this profile enumerates from it rather than offering a list
				// compiled into the binary, and the operator gets a Refresh control
				// that reaches the account.
				CatalogShape: anthropicprovider.CatalogOpenAI,
			})
		},
	},
	// No OperationPathPrefix: MiniMax serves /v1/chat/completions and
	// /v1/responses directly under the base URL, which is the default join. The
	// prefix exists for Bedrock Mantle's second route and nothing else.
	domain.ProfileMiniMaxChat: {
		authorize: staticHeader("Authorization", "Bearer ", "x-api-key"),
		build:     miniMaxOpenAIAdapter(false),
	},
	domain.ProfileMiniMaxResponses: {
		authorize: staticHeader("Authorization", "Bearer ", "x-api-key"),
		build:     miniMaxOpenAIAdapter(true),
	},

	// Bedrock Runtime and Agent Runtime. Withheld from every write path today,
	// so no connection can be created on them; the rows stay because the
	// profiles are still implemented and withholding scopes what an operator may
	// reach rather than retiring the profile.
	domain.ProfileBedrockConverseText:        {authorize: bedrockSigV4, build: bedrockRuntimeAdapter},
	domain.ProfileBedrockInvokeTitanEmbedV2:  {authorize: bedrockSigV4, build: bedrockRuntimeAdapter},
	domain.ProfileBedrockInvokeTitanImageV2:  {authorize: bedrockSigV4, build: bedrockRuntimeAdapter},
	domain.ProfileBedrockAsyncNovaReel:       {authorize: bedrockSigV4, build: bedrockRuntimeAdapter},
	domain.ProfileBedrockAgentRerankCohere35: {authorize: bedrockSigV4, build: bedrockRuntimeAdapter},

	// Bedrock Mantle. The chat pair and the responses pair differ only in the
	// route they address; the prefix is a property of the profile, never of the
	// model identifier.
	domain.ProfileBedrockMantleChat: {
		validateEndpoint: bedrockmantleprovider.ValidateEndpoint,
		authorize:        staticHeader("Authorization", "Bearer ", "api-key", "x-api-key"),
		build:            mantleOpenAIAdapter,
	},
	domain.ProfileBedrockMantleOpenAIChat: {
		validateEndpoint: bedrockmantleprovider.ValidateEndpoint,
		authorize:        staticHeader("Authorization", "Bearer ", "api-key", "x-api-key"),
		build:            mantleOpenAIAdapter,
	},
	domain.ProfileBedrockMantleResponses: {
		validateEndpoint: bedrockmantleprovider.ValidateEndpoint,
		authorize:        staticHeader("Authorization", "Bearer ", "api-key", "x-api-key"),
		build:            mantleResponsesAdapter,
	},
	domain.ProfileBedrockMantleOpenAIResponses: {
		validateEndpoint: bedrockmantleprovider.ValidateEndpoint,
		authorize:        staticHeader("Authorization", "Bearer ", "api-key", "x-api-key"),
		build:            mantleResponsesAdapter,
	},
	domain.ProfileBedrockMantleAnthropicMessages: {
		validateEndpoint: bedrockmantleprovider.ValidateEndpoint,
		authorize:        staticHeader("x-api-key", "", "Authorization", "api-key"),
		build: func(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
			return anthropicprovider.New(anthropicprovider.Options{
				Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
				Capabilities: ctx.Binding.Capabilities, ProviderType: string(domain.ProviderBedrock),
				CredentialScheme: ctx.Binding.CredentialScheme, MessagesPath: "anthropic/v1/messages",
				BedrockProjectID: ctx.Instance.BedrockProjectID, ProfileID: ctx.Binding.ProfileID,
			})
		},
	},
}

func openAICompatibleAdapter(responses bool) func(adapterBuildContext, provider.Authorizer) (provider.Adapter, error) {
	return func(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
		return openaiprovider.NewWithOptions(openaiprovider.Options{
			Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
			ProviderType: string(ctx.Instance.Type), Capabilities: ctx.Binding.Capabilities,
			Responses: responses,
		})
	}
}

func miniMaxOpenAIAdapter(responses bool) func(adapterBuildContext, provider.Authorizer) (provider.Adapter, error) {
	return func(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
		return openaiprovider.NewWithOptions(openaiprovider.Options{
			Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
			ProviderType: string(domain.ProviderMiniMax), CredentialScheme: ctx.Binding.CredentialScheme,
			Capabilities: ctx.Binding.Capabilities, Responses: responses,
		})
	}
}

func bedrockRuntimeAdapter(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
	return bedrockprovider.New(bedrockprovider.Options{
		Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client, ProfileID: ctx.Binding.ProfileID,
	})
}

func mantleOpenAIAdapter(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
	return openaiprovider.NewWithOptions(openaiprovider.Options{
		Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
		ProviderType: string(domain.ProviderBedrock), CredentialScheme: ctx.Binding.CredentialScheme,
		Capabilities: ctx.Binding.Capabilities, BedrockProjectID: ctx.Instance.BedrockProjectID,
		OperationPathPrefix: mantleOperationPathPrefix(ctx.Binding.ProfileID),
	})
}

func mantleResponsesAdapter(ctx adapterBuildContext, authorizer provider.Authorizer) (provider.Adapter, error) {
	return bedrockmantleprovider.NewResponses(bedrockmantleprovider.ResponsesOptions{
		Endpoint: ctx.Endpoint, Authorizer: authorizer, Client: ctx.Client,
		Capabilities: ctx.Binding.Capabilities, BedrockProjectID: ctx.Instance.BedrockProjectID,
		OperationPathPrefix: mantleOperationPathPrefix(ctx.Binding.ProfileID),
	})
}

// buildProviderAdapter looks the profile up and runs its three stages, closing
// whatever was already built when a later one fails.
func buildProviderAdapter(ctx adapterBuildContext) (provider.Adapter, error) {
	fail := func(err error, authorizer provider.Authorizer) (provider.Adapter, error) {
		if authorizer != nil {
			authorizer.Close()
		}
		ctx.Client.CloseIdleConnections()
		return nil, err
	}
	builder, ok := adapterBuilders[ctx.Binding.ProfileID]
	if !ok {
		// Named rather than generic: the operator's connection points at a profile
		// this build knows and cannot construct, which is a wiring defect and not
		// a bad request.
		return fail(errors.New("provider profile is not implemented: "+string(ctx.Binding.ProfileID)), nil)
	}
	if builder.validateEndpoint != nil {
		if err := builder.validateEndpoint(ctx.Endpoint); err != nil {
			return fail(err, nil)
		}
	}
	authorizer, err := builder.authorize(ctx)
	if err != nil {
		return fail(err, nil)
	}
	adapter, err := builder.build(ctx, authorizer)
	if err != nil {
		return fail(err, authorizer)
	}
	return adapter, nil
}
