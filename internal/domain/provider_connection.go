package domain

// How one connection's capabilities are shared out among the profiles that can
// serve them.
//
// A connection is anchored on one profile — the one an operator picked, or the
// type's default — and may carry more: an OpenAI key serves both the chat
// endpoints and the media ones, a Bedrock credential serves Converse and Titan
// and Nova. What the operator declares is one flat capability set, and the split
// into bindings happens here, once, in the same table the ceilings come from.
//
// It used to happen in the console, which meant the rule for "which profile
// serves images" existed in TypeScript and nowhere else, and the server accepted
// whatever bindings arrived. A caller that is not the console had to reproduce
// the split to get a connection the console could have made.
//
// The rules, in the order they resolve:
//
//   - Candidates are the profiles sharing the anchor's provider type, access
//     surface, and credential scheme, because the server requires every binding
//     on a connection to match its credential. The anchor comes first.
//   - A capability goes to the anchor whenever the anchor can serve it. This is
//     what makes an explicit choice mean something: an operator who selected the
//     Bedrock Mantle Responses implementation gets chat on that implementation,
//     not on whichever peer happens to be listed first.
//   - Otherwise it goes to the one peer that can serve it. If several peers can
//     and the anchor cannot, the request is refused rather than resolved by
//     table order — the flat set cannot say which was meant, and picking one
//     silently would bind the connection to a protocol the operator never chose.
//   - A capability no candidate can serve is refused, never dropped.
//
// The two token limits follow a rule of their own, because they are a property
// of the profile rather than of the connection: each binding takes its own
// profile's bound, and a requested value can only narrow a profile that already
// has one. It never travels to a profile that does not.
//
// That last clause is the whole point. Only Titan Embed bounds anything (8192
// context tokens), and a Bedrock connection that carries it summarises to 8192
// at the connection level, because the summary is the loosest value across the
// bindings. Applying a requested value to every binding therefore capped the
// Converse chat binding at Titan's bound the moment anyone read a connection
// back and saved it again — the console does exactly that on every edit. The
// value looked like the operator's declaration and was never anybody's.
//
// A requested bound that no profile here can hold is refused by name rather
// than dropped: the connection would otherwise report itself bounded when
// nothing is. Model token specifications belong to the Deployment, which
// declares them per model.

// ProfileCapabilityAssignment is one binding's worth of the result: the profile
// that will serve these capabilities on the connection.
type ProfileCapabilityAssignment struct {
	ProfileID    ProviderProfileID
	Capabilities ProviderCapabilities
}

// ConnectionAssignment is what a flat capability set resolves to. The three
// refusal lists are named capability keys, so a refusal can say which capability
// was the problem instead of restating the whole set.
type ConnectionAssignment struct {
	Assignments []ProfileCapabilityAssignment
	Unservable  []string
	Ambiguous   []string
	// Unboundable is a token limit no profile on this connection can hold. A
	// bound only exists where a profile declares one; asking for one anywhere
	// else has nowhere to be stored, and storing nothing quietly would report a
	// connection as bounded when it is not.
	Unboundable []string
	// Exceeded is a token limit larger than what the profile that would hold it
	// allows. Separate from Unboundable because the fix is different — lower the
	// number, rather than stop asking — and separate from Unservable because
	// "cannot serve maximum context tokens" describes neither.
	Exceeded []string
}

// ConnectionProfiles returns the profiles one connection anchored on this
// profile may carry, the anchor first.
//
// An unregistered anchor, or one that does not belong to this provider type,
// carries nothing: the caller has already failed profile resolution and should
// not be handed a group to fall back on.
func ConnectionProfiles(providerType ProviderType, anchor ProviderProfileID) []ProviderProfileSummary {
	row, ok := profileIndex[anchor]
	if !ok || row.Type != providerType {
		return nil
	}
	group := []ProviderProfileSummary{summaryOf(row)}
	for _, peer := range profileTable {
		if peer.ID == anchor || peer.Type != providerType ||
			peer.Surface != row.Surface || peer.Scheme != row.Scheme {
			continue
		}
		group = append(group, summaryOf(peer))
	}
	return group
}

// AssignConnectionCapabilities splits a flat capability set across the profiles
// of one connection.
func AssignConnectionCapabilities(
	providerType ProviderType,
	anchor ProviderProfileID,
	requested ProviderCapabilities,
) ConnectionAssignment {
	candidates := ConnectionProfiles(providerType, anchor)
	if len(candidates) == 0 {
		return ConnectionAssignment{Unservable: enabledCapabilityNames(requested)}
	}
	assigned := make([]ProviderCapabilities, len(candidates))
	result := ConnectionAssignment{}
	// Whether any profile that ends up bound can hold each token limit at all.
	var bounded struct{ MaxContextTokens, MaxOutputTokens bool }
	for _, name := range capabilityNames {
		if !capabilityEnabled(requested, name) {
			continue
		}
		home, ambiguous := homeForCapability(candidates, name)
		switch {
		case ambiguous:
			result.Ambiguous = append(result.Ambiguous, name)
		case home < 0:
			result.Unservable = append(result.Unservable, name)
		default:
			setCapabilityEnabled(&assigned[home], name, true)
		}
	}
	for index, candidate := range candidates {
		// Any capability at all, not any operation: a profile that received only
		// modifiers — tools without chat — becomes a binding that the domain write
		// boundary then refuses by name. Skipping it here instead would drop what
		// the caller asked for and store a connection that quietly does less.
		if len(enabledCapabilityNames(assigned[index])) == 0 {
			continue
		}
		if exceeds(requested.MaxContextTokens, candidate.Ceiling.MaxContextTokens) {
			result.Exceeded = append(result.Exceeded, "max_context_tokens")
		}
		if exceeds(requested.MaxOutputTokens, candidate.Ceiling.MaxOutputTokens) {
			result.Exceeded = append(result.Exceeded, "max_output_tokens")
		}
		assigned[index].MaxContextTokens = boundedLimit(requested.MaxContextTokens, candidate.Ceiling.MaxContextTokens)
		assigned[index].MaxOutputTokens = boundedLimit(requested.MaxOutputTokens, candidate.Ceiling.MaxOutputTokens)
		bounded.MaxContextTokens = bounded.MaxContextTokens || candidate.Ceiling.MaxContextTokens > 0
		bounded.MaxOutputTokens = bounded.MaxOutputTokens || candidate.Ceiling.MaxOutputTokens > 0
		result.Assignments = append(result.Assignments, ProfileCapabilityAssignment{
			ProfileID: candidate.ID, Capabilities: assigned[index],
		})
	}
	// Asked for a bound that nothing here holds. Refused rather than dropped: a
	// caller who set it and got 201 would believe the connection was bounded.
	if requested.MaxContextTokens > 0 && !bounded.MaxContextTokens {
		result.Unboundable = append(result.Unboundable, "max_context_tokens")
	}
	if requested.MaxOutputTokens > 0 && !bounded.MaxOutputTokens {
		result.Unboundable = append(result.Unboundable, "max_output_tokens")
	}
	return result
}

// exceeds reports a requested bound larger than what the profile allows. It is
// refused rather than clamped: a connection silently narrowed to a bound the
// caller did not choose reads, from every screen afterwards, as though they had.
func exceeds(requested, ceiling int64) bool {
	return requested > 0 && ceiling > 0 && requested > ceiling
}

// ConnectionCeiling is what an operator may turn on for a connection anchored
// here: what the anchor serves, plus what exactly one peer serves.
//
// A capability several peers could serve is left out rather than offered,
// because AssignConnectionCapabilities would refuse it — a form built from this
// cannot offer a tick whose save is refused, which is the whole reason the
// matrix is served rather than copied.
func ConnectionCeiling(providerType ProviderType, anchor ProviderProfileID) ProviderCapabilities {
	candidates := ConnectionProfiles(providerType, anchor)
	var ceiling ProviderCapabilities
	for _, name := range capabilityNames {
		if home, ambiguous := homeForCapability(candidates, name); home >= 0 && !ambiguous {
			setCapabilityEnabled(&ceiling, name, true)
		}
	}
	// The token limits stay zero, and that is not "unbounded" — it is "this is
	// not a connection-level question". Whether a bound can be declared at all
	// depends on which capabilities are enabled, because it is held by the
	// profile that serves them: a Bedrock connection accepts 8192 once
	// embeddings are on and refuses it otherwise. A single number here would be
	// a promise this endpoint cannot keep either way. Each profile's own bound
	// is served next to it, in Ceiling.
	return ceiling
}

// ConnectionDefaults is what a new connection anchored here starts with: the
// anchor profile's own defaults, and nothing else.
//
// Not the union across the group, which is what the console did while it owned
// this: a new Bedrock connection then started with embeddings, image generation
// and async video already ticked, because the Converse profile shares a
// credential with Titan and Nova. Saving that form unchanged declared to the
// router that this connection serves models the AWS account may have no access
// to, and it made the "capability implementation" selector inert — every anchor
// in the group produced the same pre-ticked set. The ceiling still offers the
// peers' capabilities; turning one on is now something the operator does.
//
// Token limits are deliberately zero. They are filled per binding from the
// profile that receives the capability, so a form neither shows nor sends them.
func ConnectionDefaults(providerType ProviderType, anchor ProviderProfileID) ProviderCapabilities {
	ceiling := ConnectionCeiling(providerType, anchor)
	candidates := ConnectionProfiles(providerType, anchor)
	var defaults ProviderCapabilities
	if len(candidates) == 0 {
		return defaults
	}
	for _, name := range capabilityNames {
		if capabilityEnabled(candidates[0].Defaults, name) && capabilityEnabled(ceiling, name) {
			setCapabilityEnabled(&defaults, name, true)
		}
	}
	return defaults
}

// homeForCapability answers which candidate serves a capability: the anchor if
// it can, otherwise the single peer that can. The second return says several
// peers could, which is the refusal case rather than a choice to make here.
func homeForCapability(candidates []ProviderProfileSummary, name string) (int, bool) {
	if len(candidates) == 0 {
		return -1, false
	}
	if capabilityEnabled(candidates[0].Ceiling, name) {
		return 0, false
	}
	home := -1
	for index, candidate := range candidates[1:] {
		if !capabilityEnabled(candidate.Ceiling, name) {
			continue
		}
		if home >= 0 {
			return -1, true
		}
		home = index + 1
	}
	return home, false
}

// boundedLimit is one binding's token limit. A profile with no bound of its own
// stays unbounded whatever the request says: the request cannot invent a bound
// for a profile, only lower one the profile already declares. Zero is unbounded
// on both sides, and a request above the profile's bound never reaches here —
// exceeds refuses it first.
func boundedLimit(requested, ceiling int64) int64 {
	if ceiling == 0 {
		return 0
	}
	if requested <= 0 {
		return ceiling
	}
	return requested
}

func summaryOf(row profileRow) ProviderProfileSummary {
	return ProviderProfileSummary{
		ID: row.ID, Type: row.Type, AccessSurface: row.Surface,
		CredentialScheme: row.Scheme, BaseURLTemplate: row.BaseURLTemplate,
		Immutable: row.Immutable, Defaults: row.Defaults, Ceiling: row.Ceiling,
	}
}

func enabledCapabilityNames(capabilities ProviderCapabilities) []string {
	var names []string
	for _, name := range capabilityNames {
		if capabilityEnabled(capabilities, name) {
			names = append(names, name)
		}
	}
	return names
}

func setCapabilityEnabled(c *ProviderCapabilities, name string, value bool) {
	switch name {
	case "chat":
		c.Chat = value
	case "streaming":
		c.Streaming = value
	case "embeddings":
		c.Embeddings = value
	case "tools":
		c.Tools = value
	case "vision":
		c.Vision = value
	case "fetched_image":
		c.FetchedImage = value
	case "json_mode":
		c.JSONMode = value
	case "developer_role":
		c.DeveloperRole = value
	case "reasoning":
		c.Reasoning = value
	case "stream_usage":
		c.StreamUsage = value
	case "provider_executed_tools":
		c.ProviderExecutedTools = value
	case "moderations":
		c.Moderations = value
	case "images":
		c.Images = value
	case "transcriptions":
		c.Transcriptions = value
	case "speech":
		c.Speech = value
	case "files":
		c.Files = value
	case "batches":
		c.Batches = value
	case "rerank":
		c.Rerank = value
	case "async_generate":
		c.AsyncGenerate = value
	}
}
