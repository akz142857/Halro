package semantic

import "errors"

// Operation is a provider-neutral behavior used for routing, governance, and
// accounting. Wire endpoint names and provider API names intentionally do not
// belong in this enum.
type Operation string

const (
	OperationGenerate Operation = "generate"
	OperationEmbed    Operation = "embed"
)

func (operation Operation) Validate() error {
	switch operation {
	case OperationGenerate, OperationEmbed:
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
