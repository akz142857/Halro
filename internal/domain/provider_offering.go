package domain

import "strings"

// What an operator bought, and where their account lives.
//
// The provider matrix in provider_table.go answers "what can this build speak
// to". It cannot answer "which product is this key for", and until now nothing
// did: a credential carried a provider type, an Access Surface and a credential
// scheme, and the console asked for the type alone. That was enough while every
// vendor had one product. It stopped being enough the moment one vendor sold
// two — a metered API and a Coding Plan on the same host — and it was already
// wrong for BigModel, whose mainland and global accounts are separate products
// with separate balances that the form could not tell apart.
//
// So there are two facts here, and they are declared per Access Surface rather
// than per profile:
//
//	Offering — which upstream product a credential belongs to.
//	Region   — which account/balance/catalogue boundary it sits behind.
//
// Per surface and not per profile, because the surface is what a credential
// actually stores. ResolveCredentialProfile resolves (type, surface, scheme),
// and several profiles routinely share one such triple — OpenAI's chat and
// media rows, the five Mantle rows, the three Kimi rows. A profile-level
// Offering would let two profiles on one surface claim different products while
// the credential that reaches both has no field able to say which, which is the
// second-source-of-truth this whole model exists to avoid. Declared on the
// surface, the derivation is total and one-way:
//
//	credential → (type, surface, scheme) → surfaceRow → offering + region
//	profile_id → profileRow → surface     → surfaceRow → offering + region
//
// Nothing stores an offering or a region. They are read from here.

type ProviderOfferingID string
type ProviderOfferingKind string
type ProviderRegionID string
type ProviderRegionScope string

// Kind is for display and governance and never for behaviour. What a request
// does is decided by the profile's surface, scheme, adapter builder and
// primitive bindings, exactly as before; a subscription that happened to be
// mislabelled metered must not thereby change endpoint or authentication.
//
// Every Offering in this build is metered, so nothing distinguishes on Kind
// yet. It is carried rather than deferred because it is what an Offering *is* —
// the classification the whole model turns on — and its value is a fact for
// every row today, unlike the subscription-only fields the table deliberately
// does not have yet (see providerOfferingTable). Whatever reads it first should
// be presentation.
const (
	OfferingKindMeteredAPI   ProviderOfferingKind = "metered_api"
	OfferingKindSubscription ProviderOfferingKind = "subscription"
	OfferingKindEnterprise   ProviderOfferingKind = "enterprise"
)

const (
	OfferingOpenAIAPI        ProviderOfferingID = "openai.api-platform"
	OfferingAnthropicAPI     ProviderOfferingID = "anthropic.console-api"
	OfferingAzureOpenAI      ProviderOfferingID = "azure-openai.resource"
	OfferingDeepSeekAPI      ProviderOfferingID = "deepseek.api-platform"
	OfferingGeminiAPI        ProviderOfferingID = "google.gemini-api"
	OfferingBedrockRuntime   ProviderOfferingID = "aws.bedrock-runtime"
	OfferingBedrockMantle    ProviderOfferingID = "aws.bedrock-mantle"
	OfferingOpenAICompatible ProviderOfferingID = "openai-compatible.self-declared"
	OfferingKimiOpenPlatform ProviderOfferingID = "kimi.open-platform"
	OfferingMiniMaxAPI       ProviderOfferingID = "minimax.api-platform"
	OfferingBigModelGeneral  ProviderOfferingID = "bigmodel.general-api"
)

// An Offering identifier is permanent. It reaches the Admin audit trail — the
// operator's acknowledgement of a subscription's usage terms is recorded
// against it — and an audit record is append-only and integrity-checked, so a
// renamed identifier makes an existing record unreadable rather than merely
// stale. Same rule as event kind numbers and frame epochs: add, never rewrite,
// never re-point an existing name at a different product.
//
// There are deliberately no rows here for products this build cannot reach.
// `openai.codex-subscription` and `anthropic.claude-subscription` are named in
// docs/prd/provider-offering-subscription-access-plan.zh-CN.md and registered
// when they gain a surface, not before: a row no surface references is a
// constant nothing can resolve, and the invariant tests below would have to
// carry an exception for it.

// Region is a product boundary — which account, balance and model catalogue a
// credential reaches — and never a cloud region. Bedrock's us-east-1 is a
// deployment choice that lives in configuration and is substituted into
// BaseURLTemplate; it says nothing about which product was bought, so every
// Bedrock surface here is RegionScopeNone.
const (
	RegionNone   ProviderRegionID = ""
	RegionCN     ProviderRegionID = "cn"
	RegionGlobal ProviderRegionID = "global"
)

// How a surface expresses its product region, which is not one thing in
// practice and cannot be modelled as one.
//
//   - Fixed: the surface *is* the region. BigModel's mainland and global
//     accounts are two surfaces with two profiles and two capability sets, so
//     choosing the region chooses the surface.
//   - ByEndpoint: one surface serves several account regions and the credential's
//     bound URL is what separates them. Kimi and MiniMax are both this shape:
//     one contract, two hosts, keys that are not interchangeable. Their profile
//     rows say why splitting them into two surfaces would be wrong — one
//     contract, two truths, one of which goes stale.
//   - None: the product has no region axis at all.
//
// Modelling only the first would have produced a console that offers BigModel a
// region selector and Kimi none, while a Kimi operator's wrong host fails as a
// 401 that reads like a bad key.
const (
	RegionScopeNone       ProviderRegionScope = "none"
	RegionScopeFixed      ProviderRegionScope = "fixed"
	RegionScopeByEndpoint ProviderRegionScope = "by_endpoint"
)

// RegionHost maps one host an upstream publishes to the account region behind
// it.
//
// This is a recognition table, not an allowlist. profileRow's BaseURLTemplate is
// documented as a prefill and not a bound — an operator may put a corporate
// proxy or a private entry point in front of any upstream, and the outbound
// allowlist is derived from the saved connection rather than from this file. So
// a host that is not here means "unknown", which the console shows and nothing
// refuses. Turning this into a bound would brick every install that fronts an
// upstream, and would be a separate design decision.
//
// Recognition is still worth having on a surface that pins its own region: it is
// what lets `halro doctor` say that a credential sealed to the mainland surface
// is bound to the global host, which is a real defect this build shipped and
// cannot detect any other way.
type RegionHost struct {
	Region ProviderRegionID
	Host   string
}

type providerOfferingRow struct {
	ID   ProviderOfferingID
	Type ProviderType
	Kind ProviderOfferingKind
}

// Deliberately three fields.
//
// The plan also describes a per-Offering usage warning and a documentation URL,
// for the subscription products whose terms restrict which tools may use them.
// Neither is here yet, because neither has a true value yet: every Offering in
// this build is a metered API with no usage restriction to acknowledge, and the
// documentation link a subscription needs is per (offering, region) — BigModel's
// own product is documented at docs.bigmodel.cn for mainland and docs.z.ai for
// global, so one URL per Offering would be wrong for one of them. They arrive
// with the first Offering that needs them rather than as empty columns the
// console has to render around.
var providerOfferingTable = []providerOfferingRow{
	{OfferingOpenAIAPI, ProviderOpenAI, OfferingKindMeteredAPI},
	{OfferingAnthropicAPI, ProviderAnthropic, OfferingKindMeteredAPI},
	{OfferingAzureOpenAI, ProviderAzureOpenAI, OfferingKindMeteredAPI},
	{OfferingDeepSeekAPI, ProviderDeepSeek, OfferingKindMeteredAPI},
	{OfferingGeminiAPI, ProviderGemini, OfferingKindMeteredAPI},
	{OfferingBedrockRuntime, ProviderBedrock, OfferingKindMeteredAPI},
	{OfferingBedrockMantle, ProviderBedrock, OfferingKindMeteredAPI},
	{OfferingOpenAICompatible, ProviderOpenAICompatible, OfferingKindMeteredAPI},
	{OfferingKimiOpenPlatform, ProviderKimi, OfferingKindMeteredAPI},
	{OfferingMiniMaxAPI, ProviderMiniMax, OfferingKindMeteredAPI},
	{OfferingBigModelGeneral, ProviderBigModel, OfferingKindMeteredAPI},
}

type surfaceRow struct {
	Surface     AccessSurface
	Type        ProviderType
	Offering    ProviderOfferingID
	Region      ProviderRegionID
	RegionScope ProviderRegionScope
	// Hosts are the upstream's own published addresses for this surface, in the
	// order a form offers them. On a ByEndpoint surface they are the choice
	// itself and their regions differ; on a Fixed surface they all carry the
	// surface's own region and exist so a bound endpoint can be recognised as
	// belonging elsewhere. A surface with no region axis lists none: there is
	// only one surface for its type, so recognising a host would answer a
	// question nobody asks.
	Hosts []RegionHost
}

// surfaceTable is the authority for Offering and Region. Every Access Surface a
// profile names has a row here, including the surfaces whose profiles are all
// withheld: the invariant tests walk the whole profile table, and a withheld
// profile is still a profile whose product identity has to be answerable.
var surfaceTable = []surfaceRow{
	{Surface: SurfaceOpenAI, Type: ProviderOpenAI, Offering: OfferingOpenAIAPI, RegionScope: RegionScopeNone},
	{Surface: SurfaceAnthropic, Type: ProviderAnthropic, Offering: OfferingAnthropicAPI, RegionScope: RegionScopeNone},
	{Surface: SurfaceAzureOpenAI, Type: ProviderAzureOpenAI, Offering: OfferingAzureOpenAI, RegionScope: RegionScopeNone},
	{Surface: SurfaceDeepSeek, Type: ProviderDeepSeek, Offering: OfferingDeepSeekAPI, RegionScope: RegionScopeNone},
	{Surface: SurfaceOpenAICompatible, Type: ProviderOpenAICompatible, Offering: OfferingOpenAICompatible, RegionScope: RegionScopeNone},
	{Surface: SurfaceGemini, Type: ProviderGemini, Offering: OfferingGeminiAPI, RegionScope: RegionScopeNone},
	// Runtime and Agent Runtime are two API surfaces of one purchased product,
	// so they share an Offering. Mantle is a different one: different host,
	// different credential scheme, and an operator subscribes to it separately.
	{Surface: SurfaceBedrockRuntime, Type: ProviderBedrock, Offering: OfferingBedrockRuntime, RegionScope: RegionScopeNone},
	{Surface: SurfaceBedrockAgentRuntime, Type: ProviderBedrock, Offering: OfferingBedrockRuntime, RegionScope: RegionScopeNone},
	{Surface: SurfaceBedrockMantle, Type: ProviderBedrock, Offering: OfferingBedrockMantle, RegionScope: RegionScopeNone},
	{
		Surface: SurfaceMiniMax, Type: ProviderMiniMax, Offering: OfferingMiniMaxAPI,
		RegionScope: RegionScopeByEndpoint,
		Hosts: []RegionHost{
			{Region: RegionGlobal, Host: "api.minimax.io"},
			{Region: RegionCN, Host: "api.minimaxi.com"},
		},
	},
	{
		Surface: SurfaceKimi, Type: ProviderKimi, Offering: OfferingKimiOpenPlatform,
		RegionScope: RegionScopeByEndpoint,
		Hosts: []RegionHost{
			{Region: RegionGlobal, Host: "api.moonshot.ai"},
			{Region: RegionCN, Host: "api.moonshot.cn"},
		},
	},
	{
		Surface: SurfaceBigModelCNGeneral, Type: ProviderBigModel, Offering: OfferingBigModelGeneral,
		Region: RegionCN, RegionScope: RegionScopeFixed,
		Hosts: []RegionHost{{Region: RegionCN, Host: "open.bigmodel.cn"}},
	},
	{
		Surface: SurfaceBigModelGlobalGeneral, Type: ProviderBigModel, Offering: OfferingBigModelGeneral,
		Region: RegionGlobal, RegionScope: RegionScopeFixed,
		Hosts: []RegionHost{{Region: RegionGlobal, Host: "api.z.ai"}},
	},
}

var offeringIndex = func() map[ProviderOfferingID]providerOfferingRow {
	index := make(map[ProviderOfferingID]providerOfferingRow, len(providerOfferingTable))
	for _, row := range providerOfferingTable {
		index[row.ID] = row
	}
	return index
}()

var surfaceIndex = func() map[AccessSurface]surfaceRow {
	index := make(map[AccessSurface]surfaceRow, len(surfaceTable))
	for _, row := range surfaceTable {
		index[row.Surface] = row
	}
	return index
}()

// SurfaceIdentity is what a surface says about the product behind it.
type SurfaceIdentity struct {
	Surface     AccessSurface
	Type        ProviderType
	Offering    ProviderOfferingID
	Kind        ProviderOfferingKind
	Region      ProviderRegionID
	RegionScope ProviderRegionScope
	Hosts       []RegionHost
}

// IdentityForSurface answers what product and region a surface belongs to.
func IdentityForSurface(surface AccessSurface) (SurfaceIdentity, bool) {
	row, ok := surfaceIndex[surface]
	if !ok {
		return SurfaceIdentity{}, false
	}
	offering := offeringIndex[row.Offering]
	return SurfaceIdentity{
		Surface: row.Surface, Type: row.Type,
		Offering: row.Offering, Kind: offering.Kind,
		Region: row.Region, RegionScope: row.RegionScope,
		Hosts: row.Hosts,
	}, true
}

// IdentityForProfile answers the same question starting from a profile.
func IdentityForProfile(profileID ProviderProfileID) (SurfaceIdentity, bool) {
	row, ok := profileIndex[profileID]
	if !ok {
		return SurfaceIdentity{}, false
	}
	return IdentityForSurface(row.Surface)
}

// RegionForEndpoint reads a credential's or connection's endpoint back as a
// region.
//
// Unknown is a real answer and the common one on a fronted endpoint, so it is
// returned as `false` rather than as a guess. Host comparison is
// case-insensitive and ignores the port, which is what safetransport already
// normalises away.
func RegionForEndpoint(surface AccessSurface, endpoint string) (ProviderRegionID, bool) {
	row, ok := surfaceIndex[surface]
	if !ok {
		return RegionNone, false
	}
	switch row.RegionScope {
	case RegionScopeFixed:
		return row.Region, true
	case RegionScopeByEndpoint:
		host := endpointHost(endpoint)
		if host == "" {
			return RegionNone, false
		}
		for _, candidate := range row.Hosts {
			if candidate.Host == host {
				return candidate.Region, true
			}
		}
		return RegionNone, false
	default:
		return RegionNone, true
	}
}

// endpointHost pulls the lowercase host out of an endpoint without requiring it
// to parse as a URL: this is called on stored values that safetransport already
// validated, and a value it did not is one this function has nothing to say
// about.
func endpointHost(endpoint string) string {
	value := strings.TrimSpace(endpoint)
	if index := strings.Index(value, "://"); index >= 0 {
		value = value[index+3:]
	}
	if index := strings.IndexAny(value, "/?#"); index >= 0 {
		value = value[:index]
	}
	if index := strings.LastIndex(value, "@"); index >= 0 {
		value = value[index+1:]
	}
	// Strip a port, but not the colons of a bracketed IPv6 literal.
	if !strings.HasPrefix(value, "[") {
		if index := strings.LastIndex(value, ":"); index >= 0 && !strings.Contains(value[index+1:], ":") {
			value = value[:index]
		}
	}
	return strings.ToLower(value)
}

// CredentialIdentity is one product identity a credential of this provider type
// can be created for: the (surface, scheme) pair it stores, and the product that
// pair belongs to.
type CredentialIdentity struct {
	AccessSurface    AccessSurface
	CredentialScheme CredentialScheme
	Offering         ProviderOfferingID
	Region           ProviderRegionID
	RegionScope      ProviderRegionScope
	// PrimaryProfileID is the first profile of the group, which is the profile a
	// stored credential on this identity resolves to.
	PrimaryProfileID ProviderProfileID
}

// CredentialIdentities lists the distinct product identities a new credential of
// this type may take, in table order, withheld profiles excluded.
//
// This is what makes "which product is this key for" answerable without a
// per-vendor rule: a type with one identity has nothing to ask, and a type with
// two — BigModel today, every Coding Plan vendor tomorrow — cannot have it
// guessed. The Admin write path refuses to guess for the second kind; see
// resolveCredentialIdentity in internal/app.
func CredentialIdentities(providerType ProviderType) []CredentialIdentity {
	identities := make([]CredentialIdentity, 0, 2)
	for _, row := range profileTable {
		if row.Type != providerType || row.Withheld {
			continue
		}
		duplicate := false
		for _, existing := range identities {
			if existing.AccessSurface == row.Surface && existing.CredentialScheme == row.Scheme {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		identity, ok := IdentityForSurface(row.Surface)
		if !ok {
			continue
		}
		identities = append(identities, CredentialIdentity{
			AccessSurface: row.Surface, CredentialScheme: row.Scheme,
			Offering: identity.Offering, Region: identity.Region,
			RegionScope: identity.RegionScope, PrimaryProfileID: row.ID,
		})
	}
	return identities
}

// SurfaceForEndpoint answers which surface of this provider type publishes the
// given endpoint's host.
//
// Recognition only, and unknown is the ordinary answer: an operator may front
// any upstream. What it exists for is the case where the host is recognised and
// belongs to a *different* surface than the record claims — a credential sealed
// to BigModel's mainland surface while bound to api.z.ai — which is a stored
// contradiction rather than an unusual deployment.
func SurfaceForEndpoint(providerType ProviderType, endpoint string) (AccessSurface, bool) {
	host := endpointHost(endpoint)
	if host == "" {
		return "", false
	}
	for _, row := range surfaceTable {
		if row.Type != providerType {
			continue
		}
		for _, candidate := range row.Hosts {
			if candidate.Host == host {
				return row.Surface, true
			}
		}
	}
	return "", false
}
