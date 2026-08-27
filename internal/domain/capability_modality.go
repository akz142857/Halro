package domain

// Capability modalities: what a capability says about the shape of the data
// crossing the boundary, as opposed to what it says about the protocol.
//
// The two vocabularies are not isomorphic and this file is the only place that
// admits it. Halro's capabilities are operations and protocol features — chat,
// tools, json_mode; a model catalogue elsewhere in the industry describes the
// same models as inputs and outputs — text in, image in, text out. Rendering
// the second from the first needs a mapping, and a mapping written twice
// diverges, so it is written here, served to whoever renders it, and pinned by
// a test that refuses to let a capability be silently forgotten.
//
// The inverse direction already lives beside its evidence — Bedrock reads
// IMAGE+TEXT off ListFoundationModels and claims vision, Anthropic reads
// image_input and claims the same — and those stay where they are. This is the
// forward direction only: given what Halro decided a deployment may do, which
// modalities does that describe.

// ModalityDirection is which side of the exchange a modality describes.
type ModalityDirection string

const (
	ModalityInput  ModalityDirection = "input"
	ModalityOutput ModalityDirection = "output"
)

// CapabilityModality is one row of the modality view. Capabilities is any-of:
// the row is expressed when a deployment carries any capability named here.
type CapabilityModality struct {
	Direction    ModalityDirection `json:"direction"`
	Modality     string            `json:"modality"`
	Capabilities []string          `json:"capabilities"`
}

// capabilityModalities is the mapping.
//
// Two rows a previous draft carried are deliberately absent. There is no
// "input · speech": speech is text-to-audio, so it is an output modality and
// listing it as an input was a category error dressed up as a permanently
// unknown row. And there is no "output · video" derived from async_generate:
// AsyncGenerate is the asynchronous-invocation operation, and one profile
// happening to generate video through it does not make the capability mean
// video. A row that can only ever say "unknown" is not a row.
//
// Input text is spelled out rather than derived from "any operation", because
// transcriptions is an operation whose input is audio. Deriving it would have
// claimed text input for a transcription-only deployment, which is the kind of
// confident wrongness this mapping exists to avoid — worse than saying nothing.
var capabilityModalities = []CapabilityModality{
	{ModalityInput, "text", []string{"chat", "embeddings", "moderations", "images", "speech", "rerank", "async_generate"}},
	{ModalityInput, "image", []string{"vision"}},
	// Kept apart from vision on purpose: one reads bytes the request carried,
	// the other has the gateway retrieve an address on the caller's behalf.
	// Merging them would hide a capability with its own threat model.
	{ModalityInput, "fetched_image", []string{"fetched_image"}},
	{ModalityInput, "audio", []string{"transcriptions"}},

	{ModalityOutput, "text", []string{"chat"}},
	{ModalityOutput, "image", []string{"images"}},
	{ModalityOutput, "audio", []string{"speech"}},
	{ModalityOutput, "embedding", []string{"embeddings"}},
}

// nonModalCapabilities are capabilities that describe the protocol rather than
// the data. They are listed rather than left over, so that a capability added
// to the dictionary has to be placed deliberately in one list or the other and
// cannot fall through unnoticed.
var nonModalCapabilities = []string{
	"streaming", "tools", "json_mode", "developer_role", "reasoning",
	"stream_usage", "provider_executed_tools", "files", "batches",
	"max_context_tokens", "max_output_tokens",
}

// CapabilityModalities returns the mapping. The slice is copied because the
// caller renders it and a shared backing array is a mutation waiting to happen.
func CapabilityModalities() []CapabilityModality {
	rows := make([]CapabilityModality, 0, len(capabilityModalities))
	for _, row := range capabilityModalities {
		rows = append(rows, CapabilityModality{
			Direction: row.Direction, Modality: row.Modality,
			Capabilities: append([]string(nil), row.Capabilities...),
		})
	}
	return rows
}

// NonModalCapabilities returns the capabilities that express no modality.
func NonModalCapabilities() []string {
	return append([]string(nil), nonModalCapabilities...)
}
