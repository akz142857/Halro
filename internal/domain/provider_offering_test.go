package domain

import (
	"strings"
	"testing"
)

// The Offering/Region model is only worth having if it is total: every profile
// this build knows about — withheld ones included, because the profile table is
// the one enumeration — has to answer which product it belongs to, and has to
// answer it the same way through either derivation path. These tests are what
// says so.

func TestEverySurfaceUsedByAProfileHasAnIdentity(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		identity, ok := IdentityForSurface(profile.AccessSurface)
		if !ok {
			t.Fatalf("profile %s names access surface %q, which has no row in surfaceTable",
				profile.ID, profile.AccessSurface)
		}
		if identity.Type != profile.Type {
			t.Fatalf("profile %s is provider type %q but its surface %q belongs to %q",
				profile.ID, profile.Type, profile.AccessSurface, identity.Type)
		}
	}
}

func TestIdentityForProfileMatchesIdentityForItsSurface(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		fromProfile, ok := IdentityForProfile(profile.ID)
		if !ok {
			t.Fatalf("profile %s resolves to no identity", profile.ID)
		}
		fromSurface, _ := IdentityForSurface(profile.AccessSurface)
		if fromProfile.Surface != fromSurface.Surface ||
			fromProfile.Offering != fromSurface.Offering ||
			fromProfile.Region != fromSurface.Region ||
			fromProfile.RegionScope != fromSurface.RegionScope ||
			fromProfile.Kind != fromSurface.Kind ||
			fromProfile.RequiresUsageWarning != fromSurface.RequiresUsageWarning ||
			fromProfile.DocumentationURL != fromSurface.DocumentationURL ||
			fromProfile.UsagePolicyRevision != fromSurface.UsagePolicyRevision {
			t.Fatalf("profile %s derives %+v through the profile and %+v through the surface",
				profile.ID, fromProfile, fromSurface)
		}
	}
}

func TestEveryUsageRestrictedSurfaceHasItsOwnOfficialDocument(t *testing.T) {
	for _, row := range surfaceTable {
		identity, ok := IdentityForSurface(row.Surface)
		if !ok {
			t.Fatalf("surface %q has no identity", row.Surface)
		}
		if identity.RequiresUsageWarning && strings.TrimSpace(identity.DocumentationURL) == "" {
			t.Fatalf("usage-restricted surface %q has no regional official document", row.Surface)
		}
		if identity.RequiresUsageWarning && strings.TrimSpace(identity.UsagePolicyRevision) == "" {
			t.Fatalf("usage-restricted surface %q has no stable policy revision", row.Surface)
		}
		if !identity.RequiresUsageWarning && identity.DocumentationURL != "" {
			t.Fatalf("surface %q publishes a usage-warning document without requiring the warning", row.Surface)
		}
		if !identity.RequiresUsageWarning && identity.UsagePolicyRevision != "" {
			t.Fatalf("surface %q publishes a usage-policy revision without requiring the warning", row.Surface)
		}
	}
}

func TestEverySurfaceRowIsReferencedByAProfile(t *testing.T) {
	used := make(map[AccessSurface]bool)
	for _, profile := range AllProviderProfiles() {
		used[profile.AccessSurface] = true
	}
	for _, row := range surfaceTable {
		if !used[row.Surface] {
			t.Fatalf("surface %q has a row but no profile names it; a surface nothing reaches is a"+
				" constant that cannot be resolved", row.Surface)
		}
	}
}

func TestEveryOfferingRowIsReferencedByASurface(t *testing.T) {
	used := make(map[ProviderOfferingID]bool)
	for _, row := range surfaceTable {
		used[row.Offering] = true
	}
	for _, offering := range providerOfferingTable {
		if !used[offering.ID] {
			t.Fatalf("offering %q is registered but no surface belongs to it; register it when it"+
				" gains a surface, not before", offering.ID)
		}
	}
}

// Both tables are indexed by map, so a duplicated row would be silently replaced
// rather than reported — and the row that survived would be whichever came last.
func TestSurfaceAndOfferingRowsAreUnique(t *testing.T) {
	surfaces := make(map[AccessSurface]bool, len(surfaceTable))
	for _, row := range surfaceTable {
		if surfaces[row.Surface] {
			t.Fatalf("surface %q has more than one row; the index keeps only the last", row.Surface)
		}
		surfaces[row.Surface] = true
	}
	offerings := make(map[ProviderOfferingID]bool, len(providerOfferingTable))
	for _, row := range providerOfferingTable {
		if offerings[row.ID] {
			t.Fatalf("offering %q has more than one row; the index keeps only the last", row.ID)
		}
		offerings[row.ID] = true
	}
}

func TestEverySurfaceOfferingExistsAndSharesItsProviderType(t *testing.T) {
	for _, row := range surfaceTable {
		offering, ok := offeringIndex[row.Offering]
		if !ok {
			t.Fatalf("surface %q names offering %q, which is not registered", row.Surface, row.Offering)
		}
		if offering.Type != row.Type {
			t.Fatalf("surface %q is provider type %q but its offering %q belongs to %q",
				row.Surface, row.Type, row.Offering, offering.Type)
		}
		if !IsRegisteredProviderType(row.Type) {
			t.Fatalf("surface %q belongs to unregistered provider type %q", row.Surface, row.Type)
		}
	}
}

func TestRegionScopeAndRegionAgree(t *testing.T) {
	for _, row := range surfaceTable {
		switch row.RegionScope {
		case RegionScopeFixed:
			if row.Region == RegionNone {
				t.Fatalf("surface %q pins a region but names none", row.Surface)
			}
			for _, host := range row.Hosts {
				if host.Region != row.Region {
					t.Fatalf("surface %q pins region %q but lists host %q as %q; on a fixed surface"+
						" the hosts are recognition for that one region, not a second answer",
						row.Surface, row.Region, host.Host, host.Region)
				}
			}
		case RegionScopeByEndpoint:
			if row.Region != RegionNone {
				t.Fatalf("surface %q reads its region from the endpoint and also names one", row.Surface)
			}
			if len(row.Hosts) < 2 {
				t.Fatalf("surface %q reads its region from the endpoint but lists %d host(s); with"+
					" fewer than two there is nothing to read", row.Surface, len(row.Hosts))
			}
			seenHost := make(map[string]bool, len(row.Hosts))
			seenRegion := make(map[ProviderRegionID]bool, len(row.Hosts))
			for _, host := range row.Hosts {
				if host.Host != strings.ToLower(strings.TrimSpace(host.Host)) {
					t.Fatalf("surface %q lists host %q, which is not the normalised form the lookup"+
						" compares against", row.Surface, host.Host)
				}
				if seenHost[host.Host] {
					t.Fatalf("surface %q lists host %q twice", row.Surface, host.Host)
				}
				if seenRegion[host.Region] {
					t.Fatalf("surface %q maps two hosts to region %q", row.Surface, host.Region)
				}
				if host.Region == RegionNone {
					t.Fatalf("surface %q maps host %q to no region", row.Surface, host.Host)
				}
				seenHost[host.Host], seenRegion[host.Region] = true, true
			}
		case RegionScopeNone:
			if row.Region != RegionNone || len(row.Hosts) != 0 {
				t.Fatalf("surface %q has no region axis but declares one", row.Surface)
			}
		default:
			t.Fatalf("surface %q has unknown region scope %q", row.Surface, row.RegionScope)
		}
	}
}

// An operator picks a product and a region; the console has to turn that pair
// into exactly one surface. Scoped to surfaces an operator can actually reach:
// the two withheld Bedrock surfaces share one Offering and one (empty) region,
// which is correct — they are two API faces of one product — and no form ever
// has to choose between them.
func TestOfferingAndRegionResolveOneReachableSurface(t *testing.T) {
	reachable := make(map[AccessSurface]bool)
	for _, profile := range AllProviderProfiles() {
		if !profile.Withheld {
			reachable[profile.AccessSurface] = true
		}
	}
	type key struct {
		Type     ProviderType
		Offering ProviderOfferingID
		Region   ProviderRegionID
	}
	seen := make(map[key]AccessSurface)
	for _, row := range surfaceTable {
		if !reachable[row.Surface] {
			continue
		}
		identity := key{row.Type, row.Offering, row.Region}
		if previous, clash := seen[identity]; clash {
			t.Fatalf("surfaces %q and %q are both reachable as %q/%q/%q; an operator choosing a"+
				" product and a region would have no way to say which",
				previous, row.Surface, row.Type, row.Offering, row.Region)
		}
		seen[identity] = row.Surface
	}
}

// One Offering, one region shape. A product whose surfaces disagreed about
// whether the region is the surface or the endpoint could not be rendered as a
// single control.
func TestOfferingRegionScopeIsUniform(t *testing.T) {
	scopes := make(map[ProviderOfferingID]ProviderRegionScope)
	for _, row := range surfaceTable {
		if previous, seen := scopes[row.Offering]; seen && previous != row.RegionScope {
			t.Fatalf("offering %q has surfaces with region scopes %q and %q", row.Offering, previous, row.RegionScope)
		}
		scopes[row.Offering] = row.RegionScope
	}
}

func TestEveryTypeDefaultProfileResolvesAnOffering(t *testing.T) {
	for _, providerType := range AllProviderTypes() {
		defaults, ok := DefaultProviderProfile(providerType)
		if !ok {
			t.Fatalf("provider type %q has no default profile", providerType)
		}
		if _, resolved := IdentityForProfile(defaults.ProfileID); !resolved {
			t.Fatalf("the default profile %s of provider type %q resolves to no offering",
				defaults.ProfileID, providerType)
		}
	}
}

// Every registered type can have a credential created for it, and the identity
// list is what the write path and the console both read. An empty list would
// mean a type the console offers and no credential can be saved for.
func TestEveryTypeHasAtLeastOneCredentialIdentity(t *testing.T) {
	for _, providerType := range AllProviderTypes() {
		identities := CredentialIdentities(providerType)
		if len(identities) == 0 {
			t.Fatalf("provider type %q offers no credential identity", providerType)
		}
		for _, identity := range identities {
			if identity.AccessSurface == "" || identity.CredentialScheme == "" || identity.Offering == "" {
				t.Fatalf("provider type %q has an incomplete credential identity %+v", providerType, identity)
			}
			resolved, ok := ResolveCredentialProfile(providerType, identity.AccessSurface, identity.CredentialScheme)
			if !ok || resolved.ProfileID != identity.PrimaryProfileID {
				t.Fatalf("credential identity %+v of %q does not resolve back to its own primary profile",
					identity, providerType)
			}
			if IsWithheldProfile(resolved.ProfileID) {
				t.Fatalf("credential identity %+v of %q resolves to a withheld profile", identity, providerType)
			}
		}
	}
}

// The case the whole model exists for: BigModel sells two products on one
// provider type across two account regions — so its
// type cannot have a credential's product guessed.
func TestBigModelCredentialIdentitiesCoverEveryProduct(t *testing.T) {
	identities := CredentialIdentities(ProviderBigModel)
	if len(identities) != 4 {
		t.Fatalf("BigModel has %d credential identities, want 4", len(identities))
	}
	type product struct {
		Offering ProviderOfferingID
		Region   ProviderRegionID
	}
	got := map[product]AccessSurface{}
	schemes := map[AccessSurface]CredentialScheme{}
	for _, identity := range identities {
		got[product{identity.Offering, identity.Region}] = identity.AccessSurface
		schemes[identity.AccessSurface] = identity.CredentialScheme
	}
	for key, want := range map[product]AccessSurface{
		{OfferingBigModelGeneral, RegionCN}:        SurfaceBigModelCNGeneral,
		{OfferingBigModelGeneral, RegionGlobal}:    SurfaceBigModelGlobalGeneral,
		{OfferingBigModelCodingPlan, RegionCN}:     SurfaceBigModelCNCoding,
		{OfferingBigModelCodingPlan, RegionGlobal}: SurfaceBigModelGlobalCoding,
	} {
		if got[key] != want {
			t.Fatalf("%s/%s resolves to %q, want %q", key.Offering, key.Region, got[key], want)
		}
	}
	// The subscription key is its own scheme. Sharing the general one would let a
	// general key be saved against the Coding Plan surface, where it would spend
	// a different balance than the operator chose.
	if schemes[SurfaceBigModelCNCoding] == schemes[SurfaceBigModelCNGeneral] {
		t.Fatalf("the Coding Plan and the general API share credential scheme %q",
			schemes[SurfaceBigModelCNCoding])
	}
	if schemes[SurfaceBigModelGlobalCoding] == schemes[SurfaceBigModelGlobalGeneral] {
		t.Fatalf("the international Coding Plan and general API share credential scheme %q",
			schemes[SurfaceBigModelGlobalCoding])
	}
}

// Types with more than one reachable product require an explicit credential
// identity; everys and MiniMax now both do. Kimi Code remains withheld, so Kimi
// still exposes only its metered identity until the evidence gate is cleared.
func TestCredentialIdentityCountsMatchReachableProducts(t *testing.T) {
	for _, providerType := range AllProviderTypes() {
		want := 1
		switch providerType {
		case ProviderBigModel:
			want = 4
		case ProviderMiniMax:
			want = 3
		}
		if count := len(CredentialIdentities(providerType)); count != want {
			t.Fatalf("provider type %q has %d credential identities; the write path's"+
				" resolve-when-unambiguous rule and the console's selector expect %d",
				providerType, count, want)
		}
	}
}

func TestRegionForEndpointReadsTheHost(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		surface  AccessSurface
		endpoint string
		want     ProviderRegionID
		known    bool
	}{
		{"fixed surface ignores the endpoint", SurfaceBigModelGlobalGeneral, "https://proxy.internal", RegionGlobal, true},
		{"mainland Kimi", SurfaceKimi, "https://api.moonshot.cn", RegionCN, true},
		{"international Kimi", SurfaceKimi, "https://api.moonshot.ai:443/v1", RegionGlobal, true},
		{"uppercase host", SurfaceKimi, "https://API.MOONSHOT.CN", RegionCN, true},
		{"mainland MiniMax", SurfaceMiniMax, "https://api.minimaxi.com", RegionCN, true},
		{"a fronted endpoint is unknown, not wrong", SurfaceKimi, "https://gateway.example.com", RegionNone, false},
		{"a surface with no region axis is known and empty", SurfaceOpenAI, "https://api.openai.com", RegionNone, true},
		{"Kimi Code has no proven region axis", SurfaceKimiCode, "https://api.kimi.com", RegionNone, true},
		{"MiniMax subscription region is fixed", SurfaceMiniMaxCNSubscription, "https://proxy.internal", RegionCN, true},
		{"an unregistered surface answers nothing", AccessSurface("nope"), "https://api.openai.com", RegionNone, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			region, known := RegionForEndpoint(testCase.surface, testCase.endpoint)
			if region != testCase.want || known != testCase.known {
				t.Fatalf("got (%q, %v), want (%q, %v)", region, known, testCase.want, testCase.known)
			}
		})
	}
}

func TestAccountRegionBelongsToSurface(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		surface AccessSurface
		region  ProviderRegionID
		want    bool
	}{
		{"fixed exact", SurfaceMiniMaxCNSubscription, RegionCN, true},
		{"fixed empty", SurfaceMiniMaxCNSubscription, RegionNone, false},
		{"fixed other", SurfaceMiniMaxCNSubscription, RegionGlobal, false},
		{"by endpoint known", SurfaceKimi, RegionGlobal, true},
		{"by endpoint unknown proxy", SurfaceKimi, RegionNone, true},
		{"by endpoint invalid", SurfaceKimi, ProviderRegionID("mars"), false},
		{"regionless empty", SurfaceOpenAI, RegionNone, true},
		{"regionless value", SurfaceOpenAI, RegionCN, false},
		{"unknown surface", AccessSurface("missing"), RegionNone, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := AccountRegionBelongsToSurface(testCase.surface, testCase.region); got != testCase.want {
				t.Fatalf("AccountRegionBelongsToSurface(%q, %q)=%v, want %v", testCase.surface, testCase.region, got, testCase.want)
			}
		})
	}
}

// A surface that recognises hosts has to recognise its own prefill — unless the
// host belongs to more than one surface of its type, where the address is not
// the discriminator and the honest answer is "unknown". BigModel's mainland
// general API and its Coding Plan are that case: one host, two products, told
// apart by path.
func TestSurfacesRecogniseTheirOwnPrefillsUnlessTheHostIsShared(t *testing.T) {
	owners := map[string]map[AccessSurface]bool{}
	for _, row := range surfaceTable {
		for _, host := range row.Hosts {
			key := string(row.Type) + "\x00" + host.Host
			if owners[key] == nil {
				owners[key] = map[AccessSurface]bool{}
			}
			owners[key][row.Surface] = true
		}
	}
	shared := 0
	for _, profile := range AllProviderProfiles() {
		identity, ok := IdentityForSurface(profile.AccessSurface)
		if !ok || len(identity.Hosts) == 0 || profile.BaseURLTemplate == "" {
			continue
		}
		surface, known := SurfaceForEndpoint(profile.Type, profile.BaseURLTemplate)
		if len(owners[string(profile.Type)+"\x00"+endpointHost(profile.BaseURLTemplate)]) > 1 {
			shared++
			if known {
				t.Fatalf("profile %s prefills %q, a host two surfaces answer to, and it was still"+
					" resolved to %q", profile.ID, profile.BaseURLTemplate, surface)
			}
			continue
		}
		if !known || surface != profile.AccessSurface {
			t.Fatalf("profile %s prefills %q, which its own surface does not recognise (got %q, known=%v)",
				profile.ID, profile.BaseURLTemplate, surface, known)
		}
	}
	if shared == 0 {
		t.Fatal("no shared host in the table; this test no longer covers the ambiguity it was written for")
	}
}

// The stored defect this model exists to expose: a credential sealed to the
// mainland BigModel surface while bound to the global host. Both regional hosts
// now serve a general API and a Coding Plan, so a host identifies the region but
// cannot identify the surface; the product choice must always be explicit.
func TestSurfaceForEndpointSeparatesBigModelRegions(t *testing.T) {
	for _, testCase := range []struct {
		endpoint string
		want     AccessSurface
		known    bool
	}{
		// The mainland host serves both the general API and the Coding Plan, so it
		// identifies neither: the path is what separates them.
		{"https://open.bigmodel.cn", "", false},
		{"https://api.z.ai", "", false},
		{"https://api.z.ai:443/api/paas/v4", "", false},
		{"https://bigmodel.internal.example", "", false},
	} {
		surface, known := SurfaceForEndpoint(ProviderBigModel, testCase.endpoint)
		if surface != testCase.want || known != testCase.known {
			t.Fatalf("%s: got (%q, %v), want (%q, %v)", testCase.endpoint, surface, known, testCase.want, testCase.known)
		}
	}
}

func TestRegionForProviderEndpointRecognisesSharedBigModelHosts(t *testing.T) {
	for _, testCase := range []struct {
		endpoint string
		want     ProviderRegionID
		known    bool
	}{
		{"https://open.bigmodel.cn", RegionCN, true},
		{"https://api.z.ai:443/api/paas/v4", RegionGlobal, true},
		{"https://bigmodel.internal.example", RegionNone, false},
	} {
		region, known := RegionForProviderEndpoint(ProviderBigModel, testCase.endpoint)
		if region != testCase.want || known != testCase.known {
			t.Fatalf("%s: got (%q, %v), want (%q, %v)", testCase.endpoint, region, known, testCase.want, testCase.known)
		}
	}
}
