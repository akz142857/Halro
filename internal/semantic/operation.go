package semantic

import "errors"

// Operation is a provider-neutral behavior used for routing, governance, and
// accounting. Wire endpoint names and provider API names intentionally do not
// belong in this enum.
type Operation string

const (
	OperationGenerate      Operation = "generate"
	OperationEmbed         Operation = "embed"
	OperationModerate      Operation = "moderate"
	OperationImage         Operation = "image"
	OperationTranscribe    Operation = "transcribe"
	OperationSynthesize    Operation = "synthesize"
	OperationRerank        Operation = "rerank"
	OperationAsyncGenerate Operation = "async_generate"
	OperationFile          Operation = "file"
	OperationBatch         Operation = "batch"
	OperationGovernance    Operation = "governance"
	// OperationDiscovery answers a question about the gateway's own
	// configuration — which aliases a Project may name — and makes no provider
	// call. It is an operation rather than a bare route so the compatibility
	// manifest can say what the endpoint does without pretending a provider
	// profile serves it.
	OperationDiscovery Operation = "discovery"
)

// ProviderBacked reports whether the operation reaches an upstream. The two
// that do not are answered from Halro's own state, so an endpoint manifest for
// them declares no provider profiles and no coverage.
func (operation Operation) ProviderBacked() bool {
	switch operation {
	case OperationGovernance, OperationDiscovery:
		return false
	}
	return true
}

func (operation Operation) Validate() error {
	switch operation {
	case OperationGenerate, OperationEmbed, OperationModerate, OperationImage,
		OperationTranscribe, OperationSynthesize, OperationRerank,
		OperationAsyncGenerate, OperationFile, OperationBatch:
		return nil
	case OperationGovernance, OperationDiscovery:
		return nil
	default:
		return errors.New("semantic operation is invalid")
	}
}

type ExecutionMode string

const (
	ModePortable ExecutionMode = "portable"
	ModeNative   ExecutionMode = "native"
)

// Source identifies the northbound contract without importing a wire package.
// It is provenance, not a routing or provider selector.
type Source struct {
	ProfileID       string `json:"profile_id"`
	ProfileRevision uint64 `json:"profile_revision"`
}

func (source Source) Validate() error {
	if source.ProfileID == "" || source.ProfileRevision == 0 {
		return errors.New("semantic source is incomplete")
	}
	return nil
}
