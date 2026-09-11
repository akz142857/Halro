package domain

import (
	"errors"
	"strings"
)

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
// Kind is carried because it is what an Offering *is* — the classification the
// whole model turns on — and its value is a fact for every row. Whatever reads
// it first should be presentation; routing must continue to use the exact
// profile and surface.
const (
	OfferingKindMeteredAPI   ProviderOfferingKind = "metered_api"
	OfferingKindSubscription ProviderOfferingKind = "subscription"
	OfferingKindEnterprise   ProviderOfferingKind = "enterprise"
	// Entitlement is an access key that may consume a recurring plan or prepaid
	// credits behind the same upstream product boundary. It is descriptive only;
	// routing continues to be selected by the exact profile.
	OfferingKindEntitlement ProviderOfferingKind = "entitlement"
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
	// The first subscription product this build offers. Kind is what it is sold
	// as, and nothing routes on it: the path and the credential come from the
	// profile, as they do for every other product.
	OfferingBigModelCodingPlan        ProviderOfferingID = "bigmodel.coding-plan"
	OfferingKimiCode                  ProviderOfferingID = "kimi.code"
	OfferingMiniMaxSubscriptionAccess ProviderOfferingID = "minimax.subscription-access"
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
	Region                      ProviderRegionID
	Host                        string
	UsagePolicyDocumentationURL string
	UsagePolicyRevision         string
	ProductIdentityAssurance    ProductIdentityAssurance
}

type ProductIdentityAssurance string

const (
	ProductIdentityMechanicallyVerified ProductIdentityAssurance = "mechanically_verified"
	ProductIdentityOperatorDeclared     ProductIdentityAssurance = "operator_declared_unverified"
)

type providerOfferingRow struct {
	ID                   ProviderOfferingID
	Type                 ProviderType
	Kind                 ProviderOfferingKind
	RequiresUsageWarning bool
}

var providerOfferingTable = []providerOfferingRow{
	{ID: OfferingOpenAIAPI, Type: ProviderOpenAI, Kind: OfferingKindMeteredAPI},
	{ID: OfferingAnthropicAPI, Type: ProviderAnthropic, Kind: OfferingKindMeteredAPI},
	{ID: OfferingAzureOpenAI, Type: ProviderAzureOpenAI, Kind: OfferingKindMeteredAPI},
	{ID: OfferingDeepSeekAPI, Type: ProviderDeepSeek, Kind: OfferingKindMeteredAPI},
	{ID: OfferingGeminiAPI, Type: ProviderGemini, Kind: OfferingKindMeteredAPI},
	{ID: OfferingBedrockRuntime, Type: ProviderBedrock, Kind: OfferingKindMeteredAPI},
	{ID: OfferingBedrockMantle, Type: ProviderBedrock, Kind: OfferingKindMeteredAPI},
	{ID: OfferingOpenAICompatible, Type: ProviderOpenAICompatible, Kind: OfferingKindMeteredAPI},
	{ID: OfferingKimiOpenPlatform, Type: ProviderKimi, Kind: OfferingKindMeteredAPI},
	{ID: OfferingMiniMaxAPI, Type: ProviderMiniMax, Kind: OfferingKindMeteredAPI},
	{ID: OfferingBigModelGeneral, Type: ProviderBigModel, Kind: OfferingKindMeteredAPI},
	{ID: OfferingBigModelCodingPlan, Type: ProviderBigModel, Kind: OfferingKindSubscription, RequiresUsageWarning: true},
	{ID: OfferingKimiCode, Type: ProviderKimi, Kind: OfferingKindSubscription, RequiresUsageWarning: true},
	{ID: OfferingMiniMaxSubscriptionAccess, Type: ProviderMiniMax, Kind: OfferingKindEntitlement, RequiresUsageWarning: true},
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
	// DocumentationURL is scoped to the exact product surface. Subscription
	// terms are regional documents (docs.bigmodel.cn versus docs.z.ai), so
	// putting one URL on the Offering would make a multi-region product lie for
	// one of its regions.
	DocumentationURL string
	// UsagePolicyRevision is Halro's stable identifier for the regional policy
	// metadata an operator acknowledged. The upstream document may change at
	// the same URL; storing only a boolean and URL would make an old audit event
	// indistinguishable from acceptance of a later revision.
	UsagePolicyRevision      string
	ProductIdentityAssurance ProductIdentityAssurance
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
			{Region: RegionGlobal, Host: "api.minimax.io",
				UsagePolicyDocumentationURL: "https://platform.minimax.io/docs/token-plan/intro",
				UsagePolicyRevision:         "minimax-subscription-global-2026-09-10",
				ProductIdentityAssurance:    ProductIdentityOperatorDeclared},
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
	{
		// Shares its host with the mainland general surface, which is why
		// SurfaceForEndpoint refuses a host two surfaces answer to: here the path
		// is the discriminator and the address says nothing.
		Surface: SurfaceBigModelCNCoding, Type: ProviderBigModel, Offering: OfferingBigModelCodingPlan,
		Region: RegionCN, RegionScope: RegionScopeFixed,
		Hosts:               []RegionHost{{Region: RegionCN, Host: "open.bigmodel.cn"}},
		DocumentationURL:    "https://docs.bigmodel.cn/cn/coding-plan/usage-notes",
		UsagePolicyRevision: "bigmodel-coding-plan-cn-2026-09-10",
	},
	{
		// Z.AI documents a distinct international Coding endpoint and limits it
		// to its supported tools and product environments. It is registered from
		// that first-party contract even though no international subscription key
		// was available for a billable smoke test during implementation.
		Surface: SurfaceBigModelGlobalCoding, Type: ProviderBigModel, Offering: OfferingBigModelCodingPlan,
		Region: RegionGlobal, RegionScope: RegionScopeFixed,
		Hosts:               []RegionHost{{Region: RegionGlobal, Host: "api.z.ai"}},
		DocumentationURL:    "https://docs.z.ai/devpack/usage-policy",
		UsagePolicyRevision: "zai-coding-plan-global-2026-09-10",
	},
	{
		// Kimi Code currently publishes one membership endpoint for third-party
		// tools and no independently verifiable regional account boundary.
		Surface: SurfaceKimiCode, Type: ProviderKimi, Offering: OfferingKimiCode,
		RegionScope:         RegionScopeNone,
		DocumentationURL:    "https://www.kimi.com/code/docs/en/",
		UsagePolicyRevision: "kimi-code-2026-09-10",
	},
	{
		Surface: SurfaceMiniMaxCNSubscription, Type: ProviderMiniMax, Offering: OfferingMiniMaxSubscriptionAccess,
		Region: RegionCN, RegionScope: RegionScopeFixed,
		Hosts:               []RegionHost{{Region: RegionCN, Host: "api.minimax.cn"}},
		DocumentationURL:    "https://platform.minimaxi.com/docs/token-plan/intro",
		UsagePolicyRevision: "minimax-subscription-cn-2026-09-10",
	},
	{
		Surface: SurfaceMiniMaxGlobalSubscription, Type: ProviderMiniMax, Offering: OfferingMiniMaxSubscriptionAccess,
		Region: RegionGlobal, RegionScope: RegionScopeFixed,
		Hosts:                    []RegionHost{{Region: RegionGlobal, Host: "api.minimax.io"}},
		DocumentationURL:         "https://platform.minimax.io/docs/token-plan/intro",
		UsagePolicyRevision:      "minimax-subscription-global-2026-09-10",
		ProductIdentityAssurance: ProductIdentityOperatorDeclared,
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
	Surface                  AccessSurface
	Type                     ProviderType
	Offering                 ProviderOfferingID
	Kind                     ProviderOfferingKind
	Region                   ProviderRegionID
	RegionScope              ProviderRegionScope
	Hosts                    []RegionHost
	RequiresUsageWarning     bool
	DocumentationURL         string
	UsagePolicyRevision      string
	ProductIdentityAssurance ProductIdentityAssurance
}

type UsagePolicyRequirement struct {
	OfferingID               ProviderOfferingID
	AccessSurface            AccessSurface
	AccountRegion            ProviderRegionID
	DocumentationURL         string
	PolicyRevision           string
	ProductIdentityAssurance ProductIdentityAssurance
}

// UsagePolicyAcknowledgement is the durable proof that an operator accepted
// the policy attached to one exact product surface. It is deliberately not a
// boolean: an Offering may span regions whose terms change independently, and
// an old revision must never become acceptance of a new one merely because the
// record remains enabled.
//
// Credential and ProviderInstance each keep their own acknowledgement. The
// former proves the secret was admitted for this product; the latter proves the
// live connection was admitted. Loaders require both before constructing an
// adapter for a restricted surface.
type UsagePolicyAcknowledgement struct {
	OfferingID               ProviderOfferingID       `json:"offering_id"`
	AccessSurface            AccessSurface            `json:"access_surface"`
	AccountRegion            ProviderRegionID         `json:"account_region_id"`
	PolicyRevision           string                   `json:"policy_revision"`
	ProductIdentityAssurance ProductIdentityAssurance `json:"product_identity_assurance"`
}

// UsagePolicyAcknowledgementForProfile constructs an acknowledgement only for
// a restricted profile and only for the exact current revision. Callers still
// have to record who accepted it in the append-only Admin audit trail.
func UsagePolicyAcknowledgementForProfile(profileID ProviderProfileID, revision string) (*UsagePolicyAcknowledgement, bool) {
	return UsagePolicyAcknowledgementForProfileAtEndpoint(profileID, "", revision)
}

func UsagePolicyAcknowledgementForProfileAtEndpoint(profileID ProviderProfileID, endpoint, revision string) (*UsagePolicyAcknowledgement, bool) {
	requirement, ok := UsagePolicyRequirementForProfile(profileID, endpoint)
	if !ok || strings.TrimSpace(revision) != requirement.PolicyRevision {
		return nil, false
	}
	return &UsagePolicyAcknowledgement{
		OfferingID: requirement.OfferingID, AccessSurface: requirement.AccessSurface,
		AccountRegion: requirement.AccountRegion, PolicyRevision: requirement.PolicyRevision,
		ProductIdentityAssurance: requirement.ProductIdentityAssurance,
	}, true
}

// ValidateForSurface checks the immutable identity half of an acknowledgement.
// It intentionally accepts an older non-empty revision: a policy update must
// leave the stored proof readable so the loader can withhold it as stale rather
// than making the database itself invalid or silently rewriting the proof.
func (a UsagePolicyAcknowledgement) ValidateForSurface(surface AccessSurface) error {
	identity, ok := IdentityForSurface(surface)
	if !ok {
		return errors.New("usage policy acknowledgement access surface is unknown")
	}
	if a.OfferingID != identity.Offering || a.AccessSurface != identity.Surface ||
		!AccountRegionBelongsToSurface(identity.Surface, a.AccountRegion) {
		return errors.New("usage policy acknowledgement product identity is incompatible")
	}
	if strings.TrimSpace(a.PolicyRevision) == "" {
		return errors.New("usage policy acknowledgement revision is required")
	}
	if a.ProductIdentityAssurance != ProductIdentityMechanicallyVerified &&
		a.ProductIdentityAssurance != ProductIdentityOperatorDeclared {
		return errors.New("usage policy acknowledgement product identity assurance is invalid")
	}
	return nil
}

// CurrentForProfile is the activation-time fail-closed check. Legacy records
// have no acknowledgement and therefore return false for restricted products;
// migrations must not invent consent from an old audit event or enabled flag.
func (a *UsagePolicyAcknowledgement) CurrentForProfile(profileID ProviderProfileID) bool {
	return a.CurrentForProfileAtEndpoint(profileID, "")
}

func (a *UsagePolicyAcknowledgement) CurrentForProfileAtEndpoint(profileID ProviderProfileID, endpoint string) bool {
	requirement, required := UsagePolicyRequirementForProfile(profileID, endpoint)
	if !required {
		return true
	}
	return a != nil && a.ValidateForSurface(requirement.AccessSurface) == nil &&
		a.OfferingID == requirement.OfferingID && a.AccountRegion == requirement.AccountRegion &&
		a.PolicyRevision == requirement.PolicyRevision &&
		a.ProductIdentityAssurance == requirement.ProductIdentityAssurance
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
		Hosts: row.Hosts, RequiresUsageWarning: offering.RequiresUsageWarning,
		DocumentationURL: row.DocumentationURL, UsagePolicyRevision: row.UsagePolicyRevision,
		ProductIdentityAssurance: row.ProductIdentityAssurance,
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

// UsagePolicyRequirementForProfile resolves ordinary restricted products and
// endpoint-specific declaration responsibility. MiniMax Global's general and
// subscription products have the same observable host/path/auth shape; both
// therefore require the same revision acknowledgement, while the durable proof
// remains bound to the Offering the operator selected.
func UsagePolicyRequirementForProfile(profileID ProviderProfileID, endpoint string) (UsagePolicyRequirement, bool) {
	identity, ok := IdentityForProfile(profileID)
	if !ok {
		return UsagePolicyRequirement{}, false
	}
	assurance := identity.ProductIdentityAssurance
	if assurance == "" {
		assurance = ProductIdentityMechanicallyVerified
	}
	if identity.RequiresUsageWarning {
		return UsagePolicyRequirement{
			OfferingID: identity.Offering, AccessSurface: identity.Surface, AccountRegion: identity.Region,
			DocumentationURL: identity.DocumentationURL, PolicyRevision: identity.UsagePolicyRevision,
			ProductIdentityAssurance: assurance,
		}, true
	}
	if identity.RegionScope != RegionScopeByEndpoint {
		return UsagePolicyRequirement{}, false
	}
	host := endpointHost(endpoint)
	for _, candidate := range identity.Hosts {
		if candidate.Host != host || candidate.UsagePolicyRevision == "" {
			continue
		}
		assurance = candidate.ProductIdentityAssurance
		if assurance == "" {
			assurance = ProductIdentityMechanicallyVerified
		}
		return UsagePolicyRequirement{
			OfferingID: identity.Offering, AccessSurface: identity.Surface, AccountRegion: candidate.Region,
			DocumentationURL: candidate.UsagePolicyDocumentationURL,
			PolicyRevision:   candidate.UsagePolicyRevision, ProductIdentityAssurance: assurance,
		}, true
	}
	// A private proxy hides which MiniMax region and product is behind it. The
	// only mechanically distinct general endpoint is the mainland host; every
	// other endpoint on this surface keeps the Global same-wire responsibility
	// instead of turning an unrecognised host into an acknowledgement bypass.
	if identity.Surface == SurfaceMiniMax && host != "" && host != "api.minimaxi.com" {
		for _, candidate := range identity.Hosts {
			if candidate.Region == RegionGlobal && candidate.UsagePolicyRevision != "" {
				return UsagePolicyRequirement{
					OfferingID: identity.Offering, AccessSurface: identity.Surface, AccountRegion: candidate.Region,
					DocumentationURL:         candidate.UsagePolicyDocumentationURL,
					PolicyRevision:           candidate.UsagePolicyRevision,
					ProductIdentityAssurance: ProductIdentityOperatorDeclared,
				}, true
			}
		}
	}
	return UsagePolicyRequirement{}, false
}

func UsagePolicyRequirementsForProfile(profileID ProviderProfileID) []UsagePolicyRequirement {
	identity, ok := IdentityForProfile(profileID)
	if !ok {
		return nil
	}
	if requirement, required := UsagePolicyRequirementForProfile(profileID, ""); required {
		return []UsagePolicyRequirement{requirement}
	}
	var result []UsagePolicyRequirement
	for _, host := range identity.Hosts {
		if requirement, required := UsagePolicyRequirementForProfile(profileID, "https://"+host.Host); required {
			result = append(result, requirement)
		}
	}
	return result
}

// AccountRegionBelongsToSurface validates a persisted account-region snapshot
// against the product surface that produced it. Empty is a valid value for a
// by-endpoint surface because an enterprise proxy may hide the upstream region;
// fixed surfaces always have one exact region, and regionless products have
// none. Durable readers that need to accept pre-region records may explicitly
// allow an empty fixed value before calling this helper.
func AccountRegionBelongsToSurface(surface AccessSurface, region ProviderRegionID) bool {
	identity, ok := IdentityForSurface(surface)
	if !ok {
		return false
	}
	switch identity.RegionScope {
	case RegionScopeFixed:
		return region == identity.Region
	case RegionScopeByEndpoint:
		if region == RegionNone {
			return true
		}
		for _, host := range identity.Hosts {
			if host.Region == region {
				return true
			}
		}
		return false
	case RegionScopeNone:
		return region == RegionNone
	default:
		return false
	}
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
	var found AccessSurface
	for _, row := range surfaceTable {
		if row.Type != providerType {
			continue
		}
		for _, candidate := range row.Hosts {
			if candidate.Host != host {
				continue
			}
			// A host two surfaces share tells nothing apart, and this function's
			// only caller acts on "belongs somewhere else". BigModel's mainland
			// general and Coding Plan products are both served from
			// open.bigmodel.cn and differ by path, so the host is not the
			// discriminator there and answering with either would invent a
			// finding out of a legal pairing.
			if found != "" && found != row.Surface {
				return "", false
			}
			found = row.Surface
		}
	}
	// Regionless products have no RegionHost rows by definition, but their
	// immutable profile prefill is still a first-party product endpoint. Include
	// it in surface recognition so a Kimi Code key cannot be sealed to the Kimi
	// Open Platform host (or vice versa) merely because neither has a fixed
	// regional identity.
	for _, row := range profileTable {
		if row.Type != providerType || endpointHost(row.BaseURLTemplate) != host {
			continue
		}
		if found != "" && found != row.Surface {
			return "", false
		}
		found = row.Surface
	}
	return found, found != ""
}

// RegionForProviderEndpoint answers the account region published for a host,
// even when that host serves more than one product surface. BigModel is the
// motivating case: each regional host serves both General API and Coding Plan,
// so the host cannot identify a surface, but every matching surface agrees on
// the region. Conflicting regions remain unknown rather than being guessed.
func RegionForProviderEndpoint(providerType ProviderType, endpoint string) (ProviderRegionID, bool) {
	host := endpointHost(endpoint)
	if host == "" {
		return RegionNone, false
	}
	var found ProviderRegionID
	for _, row := range surfaceTable {
		if row.Type != providerType {
			continue
		}
		for _, candidate := range row.Hosts {
			if candidate.Host != host {
				continue
			}
			if found != RegionNone && found != candidate.Region {
				return RegionNone, false
			}
			found = candidate.Region
		}
	}
	return found, found != RegionNone
}
