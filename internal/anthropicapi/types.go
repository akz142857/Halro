package anthropicapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Members the gateway reads out of each nested object. A native request forwards
// everything else verbatim, so these lists are not an acceptance filter — they
// are what the portable projection is able to carry. Portable rewrites the
// request body, so anything outside them would be dropped on the way out, and
// dropping a member that constrains the request (task_budget bounds spend) is
// worse than refusing it.
var (
	knownToolMembers         = []string{"name", "description", "input_schema", "type", "strict"}
	knownOutputConfigMembers = []string{"effort", "format"}
	knownOutputFormatMembers = []string{"type", "name", "description", "schema"}
)

func unknownMembers(raw json.RawMessage, known []string) []string {
	if len(raw) == 0 {
		return nil
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return nil
	}
	var result []string
	for name := range members {
		if !slices.Contains(known, name) {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

// UnknownMembers reports tool members the portable projection cannot carry.
func (tool Tool) UnknownMembers() []string {
	return unknownMembers(tool.Raw, knownToolMembers)
}

// UnknownMembers reports output_config members the portable projection cannot
// carry, including those nested in format.
func (config OutputConfig) UnknownMembers() []string {
	result := unknownMembers(config.Raw, knownOutputConfigMembers)
	for _, name := range unknownMembers(config.Format, knownOutputFormatMembers) {
		result = append(result, "format."+name)
	}
	return result
}

const (
	VersionHeader     = "anthropic-version"
	BetaHeader        = "anthropic-beta"
	SupportedVersion  = "2023-06-01"
	RouteModeHeader   = "Halro-Route-Mode"
	MaxRequestBytes   = 4 << 20
	MaxContentBlocks  = 4096
	MaxMessages       = 100000
	MaxToolInputBytes = 1 << 20
)

type ExecutionMode string

const (
	ModePortable ExecutionMode = "portable"
	ModeNative   ExecutionMode = "native"
)

// MaxBetaTokens bounds one request's anthropic-beta header independently of the
// per-connection allowlist, so a malformed header is rejected before it is
// compared against anything.
const MaxBetaTokens = 16

// ParseBetaTokens splits an anthropic-beta header value. It normalises
// surrounding whitespace but deliberately preserves case: tokens are matched
// against a stored allowlist, and case-folding here would let a token the
// operator never accepted match one they did.
//
// Empty elements are skipped rather than refused. HTTP's list production
// tolerates them, and an SDK that builds this header by joining an accumulated
// slice emits a trailing comma the moment one entry is conditional — refusing
// that rejects a request whose token set is unambiguous.
func ParseBetaTokens(header string) ([]string, error) {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	tokens := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		tokens = append(tokens, token)
	}
	if len(tokens) > MaxBetaTokens {
		return nil, errors.New("anthropic-beta declares too many tokens")
	}
	return tokens, nil
}

func ParseExecutionMode(value string) (ExecutionMode, error) {
	switch ExecutionMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", ModePortable:
		return ModePortable, nil
	case ModeNative:
		return ModeNative, nil
	default:
		return "", errors.New("Halro-Route-Mode must be portable or native")
	}
}

type MessageRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int64           `json:"max_tokens"`
	Messages      []MessageParam  `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int64          `json:"top_k,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Thinking      json.RawMessage `json:"thinking,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	ServiceTier   string          `json:"service_tier,omitempty"`
	OutputConfig  *OutputConfig   `json:"output_config,omitempty"`
	Raw           json.RawMessage `json:"-"`
}

// OutputConfig models only the two members the gateway has to read — effort,
// which drives the reasoning requirement, and format, which drives the
// structured-output requirement. Everything else it carries (task_budget, and
// whatever a later release adds) rides through Raw untouched, so routing can
// reason about the request without the struct becoming a second copy of the
// upstream schema.
type OutputConfig struct {
	Effort string          `json:"effort,omitempty"`
	Format json.RawMessage `json:"format,omitempty"`
	Raw    json.RawMessage `json:"-"`
}

type outputConfigWire struct {
	Effort string          `json:"effort,omitempty"`
	Format json.RawMessage `json:"format,omitempty"`
}

func (config *OutputConfig) UnmarshalJSON(data []byte) error {
	var wire outputConfigWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return errors.New("invalid output_config")
	}
	*config = OutputConfig{Effort: wire.Effort, Format: wire.Format, Raw: append(json.RawMessage(nil), data...)}
	return nil
}

func (config OutputConfig) MarshalJSON() ([]byte, error) {
	if len(config.Raw) > 0 {
		return append(json.RawMessage(nil), config.Raw...), nil
	}
	return json.Marshal(outputConfigWire{Effort: config.Effort, Format: config.Format})
}

// EffortLevels is the accepted ladder. It is validated here rather than left to
// the provider so an unroutable request fails before any budget is reserved.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

func (config OutputConfig) Validate() error {
	if config.Effort != "" && !slices.Contains(EffortLevels, config.Effort) {
		return errors.New("output_config effort must be one of " + strings.Join(EffortLevels, ", "))
	}
	if len(config.Format) == 0 {
		return nil
	}
	if !json.Valid(config.Format) || bytes.TrimSpace(config.Format)[0] != '{' {
		return errors.New("output_config format must be an object")
	}
	var format struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(config.Format, &format); err != nil {
		return errors.New("invalid output_config format")
	}
	switch format.Type {
	case "text":
		return nil
	case "json_schema":
		if len(format.Schema) == 0 || !json.Valid(format.Schema) {
			return errors.New("output_config json_schema format requires a schema")
		}
		return nil
	default:
		return errors.New("output_config format type is not supported")
	}
}

type MessageParam struct {
	Role    string        `json:"role"`
	Content ContentBlocks `json:"content"`
}

type ContentBlocks []ContentBlock

func (blocks *ContentBlocks) UnmarshalJSON(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("message content is required")
	}
	var text string
	if json.Unmarshal(data, &text) == nil {
		*blocks = []ContentBlock{{Type: "text", Text: text, Raw: append(json.RawMessage(nil), data...)}}
		return nil
	}
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(data, &rawBlocks); err != nil {
		return errors.New("message content must be a string or array")
	}
	if len(rawBlocks) > MaxContentBlocks {
		return errors.New("message contains too many content blocks")
	}
	result := make([]ContentBlock, 0, len(rawBlocks))
	for _, raw := range rawBlocks {
		block, err := decodeContentBlock(raw)
		if err != nil {
			return err
		}
		result = append(result, block)
	}
	*blocks = result
	return nil
}

func (blocks ContentBlocks) MarshalJSON() ([]byte, error) {
	values := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		if len(block.Raw) > 0 {
			values = append(values, append(json.RawMessage(nil), block.Raw...))
			continue
		}
		value, err := block.wireValue()
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		values = append(values, encoded)
	}
	return json.Marshal(values)
}

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func decodeContentBlock(raw json.RawMessage) (ContentBlock, error) {
	var block ContentBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		return ContentBlock{}, errors.New("invalid content block")
	}
	block.Raw = append(json.RawMessage(nil), raw...)
	if strings.TrimSpace(block.Type) == "" {
		return ContentBlock{}, errors.New("content block type is required")
	}
	return block, nil
}

// wireValue renders a block Halro constructed itself — one with no original
// bytes to forward. Every accepted type needs a case here: the old default
// emitted `{"type": ...}` and nothing else, which turned a portable image block
// into an image block with no source. Anthropic rejects that, and Validate could
// not see it because Validate reads the struct while the provider reads these
// bytes. A type with no case is a programming error, so it is returned as one
// rather than sent.
func (block ContentBlock) wireValue() (any, error) {
	switch block.Type {
	case "text":
		return struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{block.Type, block.Text}, nil
	case "image", "document":
		if len(block.Source) == 0 {
			return nil, errors.New(block.Type + " block requires a source")
		}
		return struct {
			Type   string          `json:"type"`
			Source json.RawMessage `json:"source"`
		}{block.Type, block.Source}, nil
	case "tool_use":
		return struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{block.Type, block.ID, block.Name, block.Input}, nil
	case "tool_result":
		return struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content,omitempty"`
			IsError   bool            `json:"is_error,omitempty"`
		}{block.Type, block.ToolUseID, block.Content, block.IsError}, nil
	case "thinking":
		return struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{block.Type, block.Thinking, block.Signature}, nil
	case "redacted_thinking":
		return struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{block.Type, block.Data}, nil
	default:
		return nil, fmt.Errorf("content block %q cannot be rendered without its original bytes", block.Type)
	}
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Type        string          `json:"type,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

// toolWire carries the fields Halro reads. Tool itself round-trips through Raw
// so that everything else a tool declaration carries — cache_control,
// defer_loading, the computer tool's display dimensions — reaches the provider
// unchanged instead of being dropped by a struct that never modelled it. This
// mirrors how ContentBlocks already preserves block bodies.
type toolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Type        string          `json:"type,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

func (tool *Tool) UnmarshalJSON(data []byte) error {
	var wire toolWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return errors.New("invalid tool")
	}
	*tool = Tool{
		Name: wire.Name, Description: wire.Description, InputSchema: wire.InputSchema,
		Type: wire.Type, Strict: wire.Strict, Raw: append(json.RawMessage(nil), data...),
	}
	return nil
}

func (tool Tool) MarshalJSON() ([]byte, error) {
	if len(tool.Raw) > 0 {
		return append(json.RawMessage(nil), tool.Raw...), nil
	}
	return json.Marshal(toolWire{
		Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
		Type: tool.Type, Strict: tool.Strict,
	})
}

// Tool families are classified by what executes them rather than by an exact
// version string. Anthropic revisions the version suffix far more often than it
// introduces a family (text_editor_20250124 → text_editor_20250728), so
// matching the family is what stops this list needing an edit per release.
//
// The match is anchored: family, then exactly one eight-digit version. A bare
// prefix test is fail-open *inside* a known prefix — `bash_code_execution_...`
// starts with `bash_` and would be read as the client-executed shell tool, while
// it is in fact the upstream code-execution container, whose whole point is that
// Anthropic runs it. Anchoring makes every unrecognised spelling fall through to
// toolExecutionUnknown, which is refused.
var (
	clientExecutedToolFamilies   = []string{"bash", "text_editor", "memory", "computer"}
	providerExecutedToolFamilies = []string{
		"web_search", "web_fetch", "code_execution", "bash_code_execution",
		"text_editor_code_execution", "advisor", "tool_search",
	}
)

var toolVersionSuffix = regexp.MustCompile(`^[0-9]{8}$`)

// matchesToolFamily reports a type spelled exactly `<family>_<YYYYMMDD>`. The
// anchor is what disambiguates overlapping families: `bash_code_execution_20250825`
// leaves `code_execution_20250825` after the `bash_` prefix, which is not a
// version, so it never matches the client-executed `bash` family.
func matchesToolFamily(value string, families []string) bool {
	for _, family := range families {
		suffix, ok := strings.CutPrefix(value, family+"_")
		if ok && toolVersionSuffix.MatchString(suffix) {
			return true
		}
	}
	return false
}

type toolExecution int

const (
	toolExecutionUnknown toolExecution = iota
	toolExecutionCustom
	toolExecutionClient
	toolExecutionProvider
)

// ClassifyTool reports who runs a declared tool. The distinction that matters to
// the gateway is egress: a client-executed tool means the caller runs it and
// returns a tool_result, so nothing leaves Halro's transport boundary, while a
// provider-executed tool has the upstream make its own network calls outside
// SafeTransport's host allowlist.
func classifyTool(toolType string) toolExecution {
	trimmed := strings.TrimSpace(toolType)
	switch {
	case trimmed == "" || trimmed == "custom":
		return toolExecutionCustom
	case matchesToolFamily(trimmed, clientExecutedToolFamilies):
		return toolExecutionClient
	case matchesToolFamily(trimmed, providerExecutedToolFamilies):
		return toolExecutionProvider
	default:
		return toolExecutionUnknown
	}
}

// UsesProviderExecutedTools reports whether the request asks the upstream to run
// tools of its own. That is an egress decision, not a formatting one: web_search
// and code_execution make the provider originate network calls that never pass
// through SafeTransport's host allowlist, so it is gated on a capability the
// operator turns on per connection rather than on whether the struct parses.
func (request MessageRequest) UsesProviderExecutedTools() bool {
	for _, tool := range request.Tools {
		if classifyTool(tool.Type) == toolExecutionProvider {
			return true
		}
	}
	return false
}

// IsAnthropicDefined reports a tool whose behaviour the model carries rather
// than the caller's schema. These have no portable equivalent — a generic
// OpenAI-wire provider cannot run text_editor — so the portable path rejects
// them instead of translating into something the target cannot honour.
func (tool Tool) IsAnthropicDefined() bool {
	return classifyTool(tool.Type) != toolExecutionCustom
}

type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

func DecodeMessageRequest(reader io.Reader) (MessageRequest, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, MaxRequestBytes+1))
	if err != nil {
		return MessageRequest{}, err
	}
	if len(payload) > MaxRequestBytes {
		return MessageRequest{}, errors.New("request body exceeds limit")
	}
	if err := rejectDuplicateMembers(payload); err != nil {
		return MessageRequest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request MessageRequest
	if err := decoder.Decode(&request); err != nil {
		return MessageRequest{}, err
	}
	if err := ensureEOF(decoder); err != nil {
		return MessageRequest{}, err
	}
	request.Raw = append(json.RawMessage(nil), payload...)
	if err := request.Validate(); err != nil {
		return MessageRequest{}, err
	}
	return request, nil
}

// rejectDuplicateMembers fails a payload that declares the same object member
// twice.
//
// This is a security boundary, not tidiness. encoding/json resolves duplicates
// last-wins, but the native path forwards the caller's original bytes verbatim,
// so a duplicate makes the document Halro inspects and the document the provider
// receives two different things: {"user_id":"<secret>","user_id":"benign"} is
// redaction-clean when parsed and still carries the secret on the wire. Rejecting
// the ambiguity is the only way to keep inspected and forwarded identical without
// re-authoring the bytes.
func rejectDuplicateMembers(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	return walkForDuplicateMembers(decoder)
}

func walkForDuplicateMembers(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			name, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := name.(string)
			if !ok {
				return errors.New("object member name must be a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("request declares object member " + key + " more than once")
			}
			seen[key] = struct{}{}
			if err := walkForDuplicateMembers(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkForDuplicateMembers(decoder); err != nil {
				return err
			}
		}
	}
	_, err = decoder.Token()
	return err
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains multiple JSON values")
		}
		return err
	}
	return nil
}

func (request MessageRequest) Validate() error {
	if strings.TrimSpace(request.Model) == "" || request.MaxTokens < 0 || len(request.Messages) == 0 || len(request.Messages) > MaxMessages {
		return errors.New("model, max_tokens, and messages are required")
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 1) {
		return errors.New("temperature must be between 0 and 1")
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		return errors.New("top_p must be between 0 and 1")
	}
	for _, message := range request.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return errors.New("messages may only use user or assistant roles")
		}
		if len(message.Content) == 0 {
			return errors.New("message content must not be empty")
		}
		for _, block := range message.Content {
			if err := validateBlock(message.Role, block); err != nil {
				return err
			}
		}
	}
	for _, tool := range request.Tools {
		switch classifyTool(tool.Type) {
		case toolExecutionCustom:
			if tool.Name == "" || len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) || bytes.TrimSpace(tool.InputSchema)[0] != '{' {
				return errors.New("invalid client tool")
			}
		case toolExecutionClient:
			// Anthropic-defined client tools are schema-less: the schema is built
			// into the model and sending one is an upstream error.
			if tool.Name == "" || len(tool.InputSchema) != 0 {
				return errors.New("Anthropic-defined client tools take a name and no input schema")
			}
		case toolExecutionProvider:
			// Accepted at the wire layer, admitted only by a route whose
			// connection declares the provider_executed_tools capability. The
			// decision belongs to the operator who owns the egress, not to the
			// decoder, so it is enforced during target selection where the
			// connection is known.
			if tool.Name == "" {
				return errors.New("provider-executed tools require a name")
			}
		default:
			return errors.New("unrecognised tool type")
		}
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Type {
		case "auto", "any":
			if request.ToolChoice.Name != "" {
				return errors.New("tool_choice name is only valid for type tool")
			}
		case "tool":
			if request.ToolChoice.Name == "" {
				return errors.New("named tool_choice requires name")
			}
		case "none":
			if request.ToolChoice.Name != "" || request.ToolChoice.DisableParallelToolUse {
				return errors.New("none tool_choice has unrelated fields")
			}
		default:
			return errors.New("invalid tool_choice type")
		}
	}
	if request.OutputConfig != nil {
		if err := request.OutputConfig.Validate(); err != nil {
			return err
		}
	}
	if len(request.Thinking) > 0 {
		var thinking struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(request.Thinking, &thinking); err != nil || strings.TrimSpace(thinking.Type) == "" {
			return errors.New("invalid thinking configuration")
		}
		if thinking.Type == "enabled" && request.ToolChoice != nil && (request.ToolChoice.Type == "any" || request.ToolChoice.Type == "tool") {
			return errors.New("enabled thinking is incompatible with forced tool_choice")
		}
	}
	return nil
}

func validateBlock(role string, block ContentBlock) error {
	switch block.Type {
	case "text":
		return nil
	case "image", "document":
		// Data-carrying input blocks. The source stays opaque on purpose: it is
		// forwarded verbatim and redaction walks the raw payload, so the only
		// thing to establish here is that a well-formed source is present.
		if role != "user" || len(block.Source) == 0 || !json.Valid(block.Source) {
			return errors.New("invalid " + block.Type + " block")
		}
	case "search_result":
		if role != "user" {
			return errors.New("invalid search_result block")
		}
	case "tool_use":
		if role != "assistant" || block.ID == "" || block.Name == "" || len(block.Input) == 0 || len(block.Input) > MaxToolInputBytes || !json.Valid(block.Input) {
			return errors.New("invalid tool_use block")
		}
	case "tool_result":
		if role != "user" || block.ToolUseID == "" || (len(block.Content) > 0 && !json.Valid(block.Content)) {
			return errors.New("invalid tool_result block")
		}
	case "thinking":
		if role != "assistant" || block.Signature == "" {
			return errors.New("thinking blocks require an unchanged signature")
		}
	case "redacted_thinking":
		if role != "assistant" || block.Data == "" {
			return errors.New("redacted thinking blocks require opaque data")
		}
	default:
		return fmt.Errorf("unsupported content block type %q", block.Type)
	}
	return nil
}

type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	ThinkingTokens           int64 `json:"thinking_tokens,omitempty"`
}

// PromptTokens is every prompt token the request consumed. Anthropic reports
// input_tokens net of both cache tiers, so recovering the full prompt span means
// adding them back — reading input_tokens alone under-counts a cached request by
// whatever fraction the cache served, which on an agent workload is most of it.
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type Message struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Content      ContentBlocks   `json:"content"`
	Model        string          `json:"model"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        Usage           `json:"usage"`
	Raw          json.RawMessage `json:"-"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
type ErrorResponse struct {
	Type      string      `json:"type"`
	Error     ErrorDetail `json:"error"`
	RequestID string      `json:"request_id,omitempty"`
}

func DecodeMessage(payload []byte) (Message, error) {
	if len(payload) == 0 || len(payload) > MaxRequestBytes {
		return Message{}, errors.New("Anthropic message response has invalid size")
	}
	var message Message
	if err := json.Unmarshal(payload, &message); err != nil {
		return Message{}, err
	}
	if message.ID == "" || message.Type != "message" || message.Role != "assistant" || message.Model == "" {
		return Message{}, errors.New("Anthropic message response is missing required fields")
	}
	for _, block := range message.Content {
		if err := validateBlock("assistant", block); err != nil {
			return Message{}, err
		}
	}
	if message.Usage.InputTokens < 0 || message.Usage.OutputTokens < 0 {
		return Message{}, errors.New("Anthropic usage is invalid")
	}
	message.Raw = append(json.RawMessage(nil), payload...)
	return message, nil
}

// TokenCount is the count_tokens response. Anthropic returns only the prompt
// span: the endpoint measures a request, so there is no completion to report.
type TokenCount struct {
	InputTokens int64 `json:"input_tokens"`
}

func DecodeTokenCount(payload []byte) (TokenCount, error) {
	if len(payload) == 0 || len(payload) > MaxRequestBytes {
		return TokenCount{}, errors.New("Anthropic count_tokens response has invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var count TokenCount
	if err := decoder.Decode(&count); err != nil {
		return TokenCount{}, err
	}
	if err := ensureEOF(decoder); err != nil {
		return TokenCount{}, err
	}
	if count.InputTokens < 0 {
		return TokenCount{}, errors.New("Anthropic count_tokens response is invalid")
	}
	return count, nil
}

type StreamEvent struct {
	Type         string       `json:"type"`
	Index        *int         `json:"index,omitempty"`
	Message      *Message     `json:"message,omitempty"`
	ContentBlock any          `json:"content_block,omitempty"`
	Delta        any          `json:"delta,omitempty"`
	Usage        *Usage       `json:"usage,omitempty"`
	Error        *ErrorDetail `json:"error,omitempty"`
}

type RawStreamEvent struct {
	Type string
	Data json.RawMessage
}
