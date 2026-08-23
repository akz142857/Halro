// Package modelcatalog holds Halro's versioned model capability catalog: the
// claim that one provider model, reached through one profile, supports one set
// of operations.
//
// A provider's /models response proves only that an identifier exists; it does
// not establish operation support, so no capability here is derived from it.
// Entries are declared in Go source (builtin.go) and reviewed like code, which
// keeps the catalog on the same release path as the profiles whose ceilings it
// must stay under. An unknown model resolves to zero capabilities rather than to
// the profile ceiling: the operator declares what it does before it can serve.
//
// Phase 0 of docs/prd/model-aware-capability-selection.zh-CN.md. Nothing in
// this package is wired into the request path yet.
package modelcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/akz142857/Halro/internal/domain"
)

// Source records where a capability claim came from. The zero value is invalid.
type Source string

const (
	// SourceBuiltin is this package's reviewed, versioned catalog.
	SourceBuiltin Source = "builtin_catalog"
	// SourceProviderMetadata is structured capability metadata returned by a
	// provider API. It is external input: it may inform a claim but never
	// pre-selects one, because that would let an upstream decide what Halro
	// enables by default.
	SourceProviderMetadata Source = "provider_metadata"
	// SourceSignedCatalog is a reviewed exact claim from the optional 1.1.0
	// background catalog reader.
	SourceSignedCatalog Source = "signed_catalog"
	// SourceVerifiedProbe is the result of an explicit side-effect-free or
	// operator-triggered verification.
	SourceVerifiedProbe Source = "verified_probe"
	// SourceOperatorDeclared is an operator's explicit declaration for a model
	// the catalog does not know.
	SourceOperatorDeclared Source = "operator_declared"
	// SourceUnsupported marks a capability that is known not to be supported or
	// that no source could establish.
	SourceUnsupported Source = "unsupported"
)

func (s Source) Valid() bool {
	switch s {
	case SourceBuiltin, SourceProviderMetadata, SourceSignedCatalog, SourceVerifiedProbe, SourceOperatorDeclared, SourceUnsupported:
		return true
	default:
		return false
	}
}

// PreselectsCapabilities reports whether a reviewed source may participate in
// an automatically resolved variant. Provider metadata and declarations still
// require their explicit resolution/onboarding paths.
func (s Source) PreselectsCapabilities() bool {
	return s == SourceBuiltin || s == SourceSignedCatalog || s == SourceVerifiedProbe
}

// MaxEvidence caps the evidence level a claim from this source may carry. A
// declaration never becomes verified by being written down confidently.
func (s Source) MaxEvidence() domain.CapabilityEvidence {
	switch s {
	case SourceVerifiedProbe:
		return domain.EvidenceVerified
	case SourceBuiltin, SourceProviderMetadata, SourceSignedCatalog, SourceOperatorDeclared:
		return domain.EvidenceDeclared
	default:
		return domain.EvidenceUnsupported
	}
}

// Status describes how well a model's capabilities are established.
type Status string

const (
	// StatusKnown means the builtin catalog covers the model.
	StatusKnown Status = "known"
	// StatusPartial means only non-builtin sources contributed.
	StatusPartial Status = "partial"
	// StatusUnknown means no source claimed anything.
	StatusUnknown Status = "unknown"
	// StatusConflicting means sources disagreed. Disputed capabilities are off.
	StatusConflicting Status = "conflicting"
)

func (s Status) Valid() bool {
	switch s {
	case StatusKnown, StatusPartial, StatusUnknown, StatusConflicting:
		return true
	default:
		return false
	}
}

// Key identifies a catalog entry. Capabilities are not a property of a model
// name alone: the same name reached through a different profile, or in a
// different region, can behave differently.
//
// Region is empty when the claim holds regardless of region. Lookup prefers an
// exact region match and falls back to the region-agnostic entry; it never
// widens a model name.
type Key struct {
	ProviderType domain.ProviderType
	Profile      domain.ProviderProfileID
	TargetKind   domain.DeploymentTargetKind
	Model        string
	Region       string
}

func (k Key) Validate() error {
	var problems []error
	if strings.TrimSpace(string(k.ProviderType)) == "" {
		problems = append(problems, errors.New("catalog key requires a provider type"))
	}
	if strings.TrimSpace(string(k.Profile)) == "" {
		problems = append(problems, errors.New("catalog key requires a profile"))
	}
	if strings.TrimSpace(k.Model) == "" {
		problems = append(problems, errors.New("catalog key requires a model"))
	}
	if k.Model != strings.TrimSpace(k.Model) || len(k.Model) > 512 {
		problems = append(problems, errors.New("catalog model must be trimmed and at most 512 bytes"))
	}
	if k.Region != strings.TrimSpace(k.Region) || len(k.Region) > 128 {
		problems = append(problems, errors.New("catalog region must be trimmed and at most 128 bytes"))
	}
	if strings.IndexFunc(k.Model, unicode.IsControl) >= 0 || strings.IndexFunc(k.Region, unicode.IsControl) >= 0 {
		problems = append(problems, errors.New("catalog model and region must not contain control characters"))
	}
	if k.TargetKind != "" && !validTargetKind(k.TargetKind) {
		problems = append(problems, errors.New("catalog target kind is not implemented"))
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	providerType, _, ok := domain.RegisteredProviderProfile(k.Profile)
	if !ok {
		return fmt.Errorf("catalog profile %q is not registered", k.Profile)
	}
	if providerType != k.ProviderType {
		return fmt.Errorf("catalog profile %q belongs to provider type %q, not %q", k.Profile, providerType, k.ProviderType)
	}
	targetKind := k.TargetKind
	if targetKind == "" {
		// Empty remains an internal shorthand for the profile's default kind.
		// Remote snapshots reject it before reaching this validation boundary.
		targetKind = defaultTargetKind(k.ProviderType, k.Profile)
	}
	if !targetKindAllowedForProfile(k.ProviderType, k.Profile, targetKind) {
		return fmt.Errorf("catalog target kind %q is incompatible with profile %q", targetKind, k.Profile)
	}
	return nil
}

func (k Key) canonical() string {
	fields := []string{string(k.ProviderType), string(k.Profile), string(k.TargetKind), k.Model, k.Region}
	var builder strings.Builder
	for _, field := range fields {
		builder.WriteString(strconv.Itoa(len(field)))
		builder.WriteByte(':')
		builder.WriteString(field)
	}
	return builder.String()
}

func validTargetKind(kind domain.DeploymentTargetKind) bool {
	switch kind {
	case domain.TargetModelID, domain.TargetAzureDeployment, domain.TargetBedrockFoundationModel,
		domain.TargetBedrockInferenceProfile, domain.TargetBedrockProvisionedThroughput, domain.TargetCustomEndpointModel:
		return true
	default:
		return false
	}
}

// targetKindAllowedForProfile validates the complete invocation identity, not
// merely membership in the global target-kind enum. Rejecting impossible
// signed identities prevents a future caller from accidentally making a
// malformed claim reachable.
func targetKindAllowedForProfile(providerType domain.ProviderType, profile domain.ProviderProfileID, kind domain.DeploymentTargetKind) bool {
	switch providerType {
	case domain.ProviderAzureOpenAI:
		return kind == domain.TargetAzureDeployment
	case domain.ProviderOpenAICompatible:
		return kind == domain.TargetCustomEndpointModel || kind == domain.TargetModelID
	case domain.ProviderBedrock:
		switch profile {
		case domain.ProfileBedrockConverseText:
			return kind == domain.TargetBedrockFoundationModel ||
				kind == domain.TargetBedrockInferenceProfile ||
				kind == domain.TargetBedrockProvisionedThroughput
		default:
			if domain.IsBedrockMantleProfile(profile) {
				return kind == domain.TargetModelID
			}
			return kind == domain.TargetBedrockFoundationModel
		}
	default:
		return kind == domain.TargetModelID
	}
}

// Ceiling returns the profile capability ceiling a claim for this key may not
// exceed.
//
// It reads the ceiling, not the defaults. The two columns differ wherever a
// profile carries something a new connection does not claim by default —
// provider-executed tools on Anthropic Messages, vision on DeepSeek — and
// bounding the catalog by the defaults made every such opt-in unstateable here.
// That is the wrong half of the table for this check: the bound a claim must
// respect is the one ProviderProfileBinding.Validate applies, and per-model
// claims are exactly what the gap between the columns exists to carry.
func (k Key) Ceiling() domain.ProviderCapabilities {
	return domain.MaxProviderCapabilitiesForProfile(k.ProviderType, k.Profile)
}

// Entry is a resolved capability claim for one key.
type Entry struct {
	Key          Key
	Status       Status
	Source       Source
	Capabilities domain.ProviderCapabilities
	// Unsupported contains exact, explicit negative claims. Absence is never
	// copied here: a remote or provider catalog must name a capability to deny
	// it, otherwise it remains unknown.
	Unsupported []string
	// Conflicts lists capability names sources disagreed about. Each is forced
	// off in Capabilities.
	Conflicts []string
}

// Unknown returns the fail-closed entry for a model no source covers: zero
// capabilities, never the profile ceiling.
func Unknown(key Key) Entry {
	// There is no capability claim to scope when nothing matched. TargetKind is
	// already part of the selection fingerprint; omitting it here keeps the
	// historical unknown-entry revision stable while exact signed lookups remain
	// kind-scoped.
	key.TargetKind = ""
	return Entry{Key: key, Status: StatusUnknown, Source: SourceUnsupported}
}

// Evidence renders the entry's capabilities as an evidence set bounded by what
// its source is allowed to claim.
func (e Entry) Evidence() domain.CapabilityEvidenceSet {
	return domain.EvidenceForCapabilities(e.Capabilities, e.Source.MaxEvidence())
}

// Revision is the per-model digest. Conflict detection compares this, not the
// catalog digest: a catalog-wide digest rotates whenever any unrelated model
// appears, and operators would learn to retry through the resulting conflicts
// until they meant nothing.
func (e Entry) Revision() string {
	sum := sha256.Sum256([]byte(e.canonical()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (e Entry) canonical() string {
	fields := []string{e.Key.canonical(), string(e.Status), string(e.Source)}
	for _, name := range CapabilityNames {
		fields = append(fields, name+"="+capabilityValue(e.Capabilities, name))
	}
	conflicts := slices.Clone(e.Conflicts)
	slices.Sort(conflicts)
	conflicts = slices.Compact(conflicts)
	unsupported := slices.Clone(e.Unsupported)
	slices.Sort(unsupported)
	unsupported = slices.Compact(unsupported)
	fields = append(fields, "unsupported="+strings.Join(unsupported, ","))
	fields = append(fields, "conflicts="+strings.Join(conflicts, ","))
	return strings.Join(fields, "\x00")
}

func (e Entry) Validate() error {
	var problems []error
	if err := e.Key.Validate(); err != nil {
		problems = append(problems, err)
	}
	if !e.Status.Valid() {
		problems = append(problems, fmt.Errorf("catalog status %q is invalid", e.Status))
	}
	if !e.Source.Valid() {
		problems = append(problems, fmt.Errorf("catalog source %q is invalid", e.Source))
	}
	if err := ValidateDependencies(e.Capabilities); err != nil {
		problems = append(problems, err)
	}
	if !domain.ProviderCapabilitiesSubset(e.Capabilities, e.Key.Ceiling()) {
		problems = append(problems, fmt.Errorf("catalog entry for %q exceeds the %q profile ceiling", e.Key.Model, e.Key.Profile))
	}
	for _, name := range e.Conflicts {
		if !slices.Contains(CapabilityNames, name) {
			problems = append(problems, fmt.Errorf("catalog conflict names unknown capability %q", name))
		}
	}
	for _, name := range e.Unsupported {
		if !slices.Contains(CapabilityNames, name) {
			problems = append(problems, fmt.Errorf("catalog entry names unknown unsupported capability %q", name))
		}
		if isLimit(name) {
			problems = append(problems, fmt.Errorf("catalog entry cannot deny numeric limit %q", name))
		}
		if HasCapability(e.Capabilities, name) {
			problems = append(problems, fmt.Errorf("catalog entry both supports and denies capability %q", name))
		}
	}
	return errors.Join(problems...)
}

// Claim is one source's assertion about a model. Supported carries what the
// source asserts is available; Unsupported names what it asserts is not.
// Keeping them apart matters: a false boolean in Supported is indistinguishable
// from "this source said nothing", and treating silence as denial would let one
// incomplete source veto another's evidence.
type Claim struct {
	Source      Source
	Supported   domain.ProviderCapabilities
	Unsupported []string
}

// Merge folds claims for one key into a single entry. Disagreement fails
// closed: a capability asserted both supported and unsupported is switched off
// and recorded in Conflicts. The result is always clamped to the profile
// ceiling, so no source can widen a profile — including a Beta profile whose
// limits are pinned deliberately.
func Merge(key Key, claims ...Claim) Entry {
	if len(claims) == 0 {
		return Unknown(key)
	}
	supported := make(map[string]bool, len(CapabilityNames))
	denied := make(map[string]bool, len(CapabilityNames))
	limits := map[string]int64{}
	builtin := false
	for _, claim := range claims {
		if claim.Source == SourceBuiltin {
			builtin = true
		}
		for _, name := range CapabilityNames {
			if isLimit(name) {
				if value := limitValue(claim.Supported, name); value > 0 {
					// Lowest declared limit wins across sources. This narrows a
					// derivation, not operator input: an operator value above
					// the resulting limit is rejected, never clamped.
					if current, ok := limits[name]; !ok || value < current {
						limits[name] = value
					}
				}
				continue
			}
			if booleanValue(claim.Supported, name) {
				supported[name] = true
			}
		}
		for _, name := range claim.Unsupported {
			denied[name] = true
		}
	}

	var conflicts []string
	for name := range supported {
		if denied[name] {
			conflicts = append(conflicts, name)
		}
	}
	slices.Sort(conflicts)
	for _, name := range conflicts {
		delete(supported, name)
	}

	capabilities := domain.ProviderCapabilities{}
	for name := range supported {
		setBoolean(&capabilities, name)
	}
	for name, value := range limits {
		setLimit(&capabilities, name, value)
	}
	capabilities = Clamp(capabilities, key.Ceiling())

	entry := Entry{Key: key, Source: primarySource(claims), Capabilities: capabilities, Conflicts: conflicts}
	switch {
	case len(conflicts) > 0:
		entry.Status = StatusConflicting
	case !capabilities.AnyOperation():
		entry.Status = StatusUnknown
		entry.Source = SourceUnsupported
	case builtin:
		entry.Status = StatusKnown
	default:
		entry.Status = StatusPartial
	}
	return entry
}

// primarySource reports the source that carries the entry, preferring the one
// allowed to claim the most.
func primarySource(claims []Claim) Source {
	best := SourceUnsupported
	for _, claim := range claims {
		if claim.Source == SourceBuiltin {
			return SourceBuiltin
		}
		if rank(claim.Source) > rank(best) {
			best = claim.Source
		}
	}
	return best
}

func rank(source Source) int {
	switch source {
	case SourceBuiltin:
		return 4
	case SourceVerifiedProbe:
		return 3
	case SourceSignedCatalog:
		return 2
	case SourceOperatorDeclared:
		return 2
	case SourceProviderMetadata:
		return 1
	default:
		return 0
	}
}

// Clamp intersects capabilities with a ceiling. Booleans are conjunctions;
// a zero limit means the layer declared none, so the other layer's limit stands.
func Clamp(candidate, ceiling domain.ProviderCapabilities) domain.ProviderCapabilities {
	result := domain.ProviderCapabilities{}
	for _, name := range CapabilityNames {
		if isLimit(name) {
			setLimit(&result, name, narrowerLimit(limitValue(candidate, name), limitValue(ceiling, name)))
			continue
		}
		if booleanValue(candidate, name) && booleanValue(ceiling, name) {
			setBoolean(&result, name)
		}
	}
	return result
}

// Union is the dual of Clamp: booleans are disjunctions and the wider limit
// wins. It exists to describe a combined claim when reporting what exceeds a
// ceiling, never to grant capabilities — no path that decides what a deployment
// may do calls it.
func Union(first, second domain.ProviderCapabilities) domain.ProviderCapabilities {
	result := domain.ProviderCapabilities{}
	for _, name := range CapabilityNames {
		if isLimit(name) {
			setLimit(&result, name, widerLimit(limitValue(first, name), limitValue(second, name)))
			continue
		}
		if booleanValue(first, name) || booleanValue(second, name) {
			setBoolean(&result, name)
		}
	}
	return result
}

func widerLimit(first, second int64) int64 {
	// A zero limit means "none declared", which is the widest claim of all.
	if first <= 0 || second <= 0 {
		return 0
	}
	if first > second {
		return first
	}
	return second
}

// GainedCapabilities reports the boolean capabilities present in after but not
// in before, in CapabilityNames order.
//
// Limits are deliberately excluded. A boolean capability is what decides whether
// a deployment is a candidate at all — both the operation filter in the registry
// and filterSemanticCapabilities in the gateway work on these names — whereas a
// token limit narrows which individual requests fit, which is a property of the
// request rather than of the candidate set.
func GainedCapabilities(before, after domain.ProviderCapabilities) []string {
	return booleanDifference(after, before)
}

// LostCapabilities reports the boolean capabilities present in before but not in
// after. This is the set a route preflight has to reason about: losing one can
// leave a public model with no candidate for requests that need it.
func LostCapabilities(before, after domain.ProviderCapabilities) []string {
	return booleanDifference(before, after)
}

// HasCapability reports whether a boolean capability is held, by the same names
// GainedCapabilities and LostCapabilities report. Limit names always answer
// false: they are not something a candidate either has or lacks.
func HasCapability(capabilities domain.ProviderCapabilities, name string) bool {
	if isLimit(name) {
		return false
	}
	return booleanValue(capabilities, name)
}

func booleanDifference(held, against domain.ProviderCapabilities) []string {
	var names []string
	for _, name := range CapabilityNames {
		if isLimit(name) {
			continue
		}
		if booleanValue(held, name) && !booleanValue(against, name) {
			names = append(names, name)
		}
	}
	return names
}

func narrowerLimit(candidate, ceiling int64) int64 {
	switch {
	case candidate <= 0:
		return ceiling
	case ceiling <= 0:
		return candidate
	case candidate < ceiling:
		return candidate
	default:
		return ceiling
	}
}

// ValidateDependencies enforces that enhancements ride on the core operation
// they modify.
//
// domain.Deployment currently enforces only streaming -> chat. The catalog holds
// the full rule so a catalog entry cannot declare, say, tools without chat;
// reconciling the two lives in Phase 1, because tightening domain validation
// rejects configurations that exist today.
func ValidateDependencies(capabilities domain.ProviderCapabilities) error {
	var problems []error
	for _, dependent := range []struct {
		name    string
		enabled bool
	}{
		{"streaming", capabilities.Streaming},
		{"tools", capabilities.Tools},
		{"vision", capabilities.Vision},
		{"fetched_image", capabilities.FetchedImage},
		{"json_mode", capabilities.JSONMode},
		{"developer_role", capabilities.DeveloperRole},
		{"reasoning", capabilities.Reasoning},
	} {
		if dependent.enabled && !capabilities.Chat {
			problems = append(problems, fmt.Errorf("%s capability requires chat capability", dependent.name))
		}
	}
	if capabilities.StreamUsage && !capabilities.Streaming {
		problems = append(problems, errors.New("stream_usage capability requires streaming capability"))
	}
	if capabilities.MaxContextTokens < 0 || capabilities.MaxOutputTokens < 0 {
		problems = append(problems, errors.New("capability token limits cannot be negative"))
	}
	if capabilities.MaxContextTokens > 0 && capabilities.MaxOutputTokens > capabilities.MaxContextTokens {
		problems = append(problems, errors.New("max output tokens cannot exceed max context tokens"))
	}
	return errors.Join(problems...)
}

// Catalog is an immutable set of entries.
type Catalog struct {
	entries  map[string]Entry
	ordered  []Entry
	revision string
}

// New validates entries and computes the catalog digest.
func New(entries ...Entry) (*Catalog, error) {
	catalog := &Catalog{entries: make(map[string]Entry, len(entries))}
	var problems []error
	for _, entry := range entries {
		if entry.Key.TargetKind == "" {
			// Internal catalog builders historically used an omitted kind as the
			// profile's unambiguous default. Normalize before validation, hashing,
			// and storage so Lookup reaches the same exact identity. Signed wire
			// entries remain stricter and reject an omitted target_kind.
			entry.Key.TargetKind = defaultTargetKind(entry.Key.ProviderType, entry.Key.Profile)
		}
		if err := entry.Validate(); err != nil {
			problems = append(problems, err)
			continue
		}
		if entry.Status == StatusUnknown {
			problems = append(problems, fmt.Errorf("catalog entry for %q claims nothing", entry.Key.Model))
			continue
		}
		if !entry.Capabilities.AnyOperation() && len(entry.Unsupported) == 0 {
			problems = append(problems, fmt.Errorf("catalog entry for %q declares no core operation", entry.Key.Model))
			continue
		}
		key := entry.Key.canonical()
		if _, exists := catalog.entries[key]; exists {
			problems = append(problems, fmt.Errorf("catalog has duplicate entry for %q", entry.Key.Model))
			continue
		}
		catalog.entries[key] = entry
		catalog.ordered = append(catalog.ordered, entry)
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	slices.SortFunc(catalog.ordered, func(left, right Entry) int {
		return strings.Compare(left.Key.canonical(), right.Key.canonical())
	})
	digest := sha256.New()
	for _, entry := range catalog.ordered {
		digest.Write([]byte(entry.Revision()))
		digest.Write([]byte{0})
	}
	catalog.revision = "sha256:" + hex.EncodeToString(digest.Sum(nil))
	return catalog, nil
}

// Lookup resolves a key to an entry. Matching is exact on the model: a prefix
// must never promote an unknown future model to known capabilities. A key whose
// region is not covered falls back to a region-agnostic entry for the same
// model, which is an explicit claim that the capability does not vary by region.
func (c *Catalog) Lookup(key Key) (Entry, bool) {
	if key.TargetKind == "" {
		key.TargetKind = defaultTargetKind(key.ProviderType, key.Profile)
	}
	if entry, ok := c.entries[key.canonical()]; ok {
		return entry, true
	}
	if key.Region != "" {
		agnosticRegion := key
		agnosticRegion.Region = ""
		if entry, ok := c.entries[agnosticRegion.canonical()]; ok {
			return entry, true
		}
	}
	return Unknown(key), false
}

// Revision is the digest of the whole catalog. It is diagnostic only.
func (c *Catalog) Revision() string { return c.revision }

// Entries returns the catalog in a stable order.
func (c *Catalog) Entries() []Entry { return slices.Clone(c.ordered) }

func (c *Catalog) Len() int { return len(c.ordered) }

// CapabilityNames is the ordered set of capability fields the catalog encodes.
// The order is fixed because entry digests depend on it; a test asserts it
// still covers every field of domain.ProviderCapabilities.
var CapabilityNames = []string{
	"chat", "embeddings", "moderations", "images", "transcriptions", "speech",
	"files", "batches", "rerank", "async_generate",
	"streaming", "tools", "vision", "fetched_image", "json_mode", "developer_role", "reasoning", "stream_usage",
	"provider_executed_tools",
	"max_context_tokens", "max_output_tokens",
}

func isLimit(name string) bool {
	return name == "max_context_tokens" || name == "max_output_tokens"
}

func capabilityValue(capabilities domain.ProviderCapabilities, name string) string {
	if isLimit(name) {
		return strconv.FormatInt(limitValue(capabilities, name), 10)
	}
	if booleanValue(capabilities, name) {
		return "1"
	}
	return "0"
}

func booleanValue(capabilities domain.ProviderCapabilities, name string) bool {
	value, _ := domain.CapabilityValue(capabilities, name)
	return value
}

func setBoolean(capabilities *domain.ProviderCapabilities, name string) {
	domain.SetCapability(capabilities, name, true)
}

func limitValue(capabilities domain.ProviderCapabilities, name string) int64 {
	switch name {
	case "max_context_tokens":
		return capabilities.MaxContextTokens
	case "max_output_tokens":
		return capabilities.MaxOutputTokens
	default:
		return 0
	}
}

func setLimit(capabilities *domain.ProviderCapabilities, name string, value int64) {
	switch name {
	case "max_context_tokens":
		capabilities.MaxContextTokens = value
	case "max_output_tokens":
		capabilities.MaxOutputTokens = value
	}
}
