package redaction

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

var (
	ErrSecretDetected = errors.New("request contains secret material")
	ErrPolicyRejected = errors.New("redaction policy rejected content")
)

const maxBoundedMatchBytes = 4096

var builtinPatterns = map[string]string{
	"china_phone":         `(?:\+?86[- ]?)?1[3-9][0-9]{9}`,
	"email":               `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
	"china_id":            `[1-9][0-9]{5}(?:19|20)[0-9]{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12][0-9]|3[01])[0-9]{3}[0-9Xx]`,
	"bank_card_candidate": `[0-9](?:[ -]?[0-9]){12,18}`,
	"gateway_key":         `gw_[A-Za-z0-9_-]{40,256}`,
	"openai_key":          `sk-[A-Za-z0-9_-]{16,256}`,
	"anthropic_key":       `sk-ant-[A-Za-z0-9_-]{16,256}`,
	"google_key":          `AIza[0-9A-Za-z_-]{20,256}`,
	"aws_access_key":      `(?:AKIA|ASIA)[0-9A-Z]{16}`,
	"bearer_token":        `(?i)Bearer[ \t]{1,8}[A-Za-z0-9._~+/=-]{16,512}`,
	"private_key":         `-----BEGIN[ \t]+(?:RSA |EC |OPENSSH )?PRIVATE KEY-----`,
}

var mandatorySecretCategories = []string{
	"gateway_key", "openai_key", "anthropic_key", "google_key",
	"aws_access_key", "bearer_token", "private_key",
}

// ErrPolicyUnavailable reports that a named policy is absent from the live
// snapshot. It is deliberately not a MatchError: nothing matched, the rules
// never ran, and treating it as a clean pass is the fail-open direction.
var ErrPolicyUnavailable = errors.New("redaction policy is unavailable")

type MatchError struct {
	RuleID   string
	Category string
	Scope    string
}

func (e *MatchError) Error() string {
	return "redaction rule rejected content"
}

func (e *MatchError) Unwrap() error {
	return ErrPolicyRejected
}

type Match struct {
	RuleID   string `json:"rule_id"`
	Category string `json:"category"`
	Action   string `json:"action"`
	Field    string `json:"field"`
}

type compiledRule struct {
	rule      domain.RedactionRule
	pattern   *regexp.Regexp
	validator func(string) bool
}

type compiledPolicy struct {
	policy domain.RedactionPolicy
	rules  []compiledRule
}

type Engine struct {
	mu        sync.RWMutex
	mandatory []compiledRule
	policies  map[string]compiledPolicy
	hits      map[string]uint64
}

func New(policies []domain.RedactionPolicy) (*Engine, error) {
	engine := &Engine{
		mandatory: make([]compiledRule, 0, len(mandatorySecretCategories)),
		policies:  make(map[string]compiledPolicy),
		hits:      make(map[string]uint64),
	}
	for _, category := range mandatorySecretCategories {
		rule, err := compileRule(domain.RedactionRule{
			ID: "mandatory_" + category, Name: category, Kind: "builtin",
			Builtin: category, Scopes: []string{"inbound", "outbound"},
			Action: "reject", Enabled: true,
		})
		if err != nil {
			return nil, err
		}
		engine.mandatory = append(engine.mandatory, rule)
	}
	if err := engine.ReplacePolicies(policies); err != nil {
		return nil, err
	}
	return engine, nil
}

func NewDefault() *Engine {
	engine, err := New(nil)
	if err != nil {
		panic(err)
	}
	return engine
}

func BuiltinCategories() []string {
	result := make([]string, 0, len(builtinPatterns))
	for category := range builtinPatterns {
		result = append(result, category)
	}
	slices.Sort(result)
	return result
}

func CompilePolicy(policy domain.RedactionPolicy) (domain.RedactionPolicy, error) {
	if err := policy.Validate(); err != nil {
		return domain.RedactionPolicy{}, err
	}
	for index := range policy.Rules {
		compiled, err := compileRule(policy.Rules[index])
		if err != nil {
			return domain.RedactionPolicy{}, fmt.Errorf(
				"compile redaction rule %q: %w", policy.Rules[index].ID, err,
			)
		}
		policy.Rules[index].ComputedMaxMatchBytes = compiled.rule.ComputedMaxMatchBytes
		if policy.Mode == "bounded_stream" &&
			policy.Rules[index].Enabled &&
			policy.Rules[index].Action != "detect_only" &&
			(compiled.rule.ComputedMaxMatchBytes <= 0 ||
				compiled.rule.ComputedMaxMatchBytes > maxBoundedMatchBytes) {
			return domain.RedactionPolicy{}, fmt.Errorf(
				"rule %q has no safe bounded streaming width", policy.Rules[index].ID,
			)
		}
		if policy.Mode == "detect_only_stream" &&
			policy.Rules[index].Enabled &&
			policy.Rules[index].Action != "detect_only" {
			return domain.RedactionPolicy{}, fmt.Errorf(
				"rule %q must use detect_only action in detect_only_stream mode",
				policy.Rules[index].ID,
			)
		}
	}
	return policy, nil
}

func (e *Engine) ReplacePolicies(policies []domain.RedactionPolicy) error {
	next := make(map[string]compiledPolicy, len(policies))
	for _, policy := range policies {
		if policy.DeletedAt != nil || !policy.Enabled {
			continue
		}
		normalized, err := CompilePolicy(policy)
		if err != nil {
			return err
		}
		compiled := compiledPolicy{policy: normalized}
		for _, rule := range normalized.Rules {
			if !rule.Enabled {
				continue
			}
			value, err := compileRule(rule)
			if err != nil {
				return err
			}
			compiled.rules = append(compiled.rules, value)
		}
		slices.SortStableFunc(compiled.rules, func(left, right compiledRule) int {
			if left.rule.Action == "reject" && right.rule.Action != "reject" {
				return -1
			}
			if right.rule.Action == "reject" && left.rule.Action != "reject" {
				return 1
			}
			return right.rule.Priority - left.rule.Priority
		})
		if _, exists := next[policy.ID]; exists {
			return errors.New("duplicate redaction policy id")
		}
		next[policy.ID] = compiled
	}
	e.mu.Lock()
	e.policies = next
	e.mu.Unlock()
	return nil
}

func (e *Engine) HasPolicy(id string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, exists := e.policies[id]
	return exists
}

func (e *Engine) AllowsStreaming(id string) bool {
	if id == "" {
		return true
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	policy, exists := e.policies[id]
	if !exists {
		return true
	}
	if policy.policy.Mode == "strict" {
		return false
	}
	return true
}

func (e *Engine) HitCounters() map[string]uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make(map[string]uint64, len(e.hits))
	for key, value := range e.hits {
		result[key] = value
	}
	return result
}

func (e *Engine) Test(policyID, input, scope string) ([]Match, error) {
	if scope != "inbound" && scope != "outbound" {
		return nil, errors.New("redaction test scope must be inbound or outbound")
	}
	policy, ok := e.policy(policyID)
	if !ok {
		return nil, errors.New("redaction policy is unavailable")
	}
	var result []Match
	for _, rule := range policy.rules {
		if hasScope(rule.rule, scope) && rule.matches(input) {
			result = append(result, Match{
				RuleID: rule.rule.ID, Category: category(rule.rule),
				Action: rule.rule.Action, Field: "input",
			})
		}
	}
	return result, nil
}

// ProcessInboundGenerate applies the Project policy to a request on its way
// upstream, on the portable representation rather than on one facade's wire
// form.
//
// It replaces a Chat-shaped pass that every facade had to become before it
// could be inspected. That worked while Chat Completions was the only shape a
// request could take, and it fixed the vocabulary at whatever Chat can express:
// a content kind the Chat wire has no member for could not reach a redactor
// that walked Chat messages. Walking the semantic content parts instead means
// one traversal covers every facade, and a new content kind has one place to be
// added rather than one place per endpoint.
//
// What is inspected is unchanged from that pass, member for member: text, the
// address an image is named by, and tool-call arguments are rewritten; a tool
// call's identifier and a message's name are refused rather than rewritten,
// because a rewritten identifier no longer refers to anything.
//
// Reasoning text is not inspected, which is also unchanged. It arrives as its
// own member rather than inside message content, and the Chat pass never
// covered it. Bringing it in is a policy change and belongs to whoever decides
// that, not to the traversal that was moved.
func (e *Engine) ProcessInboundGenerate(
	policyID string,
	request semantic.GenerateRequest,
) (semantic.GenerateRequest, error) {
	messages := make([]semantic.Message, len(request.Messages))
	copy(messages, request.Messages)
	for index := range messages {
		if err := e.validateMandatory(messages[index].Name); err != nil {
			return request, err
		}
		content, err := e.processContent(policyID, "inbound", messages[index].Content)
		if err != nil {
			return request, err
		}
		messages[index].Content = content
	}
	request.Messages = messages
	return request, nil
}

// ProcessOutboundGenerateResult is the same traversal on the way back.
func (e *Engine) ProcessOutboundGenerateResult(
	policyID string,
	result semantic.GenerateResult,
) (semantic.GenerateResult, error) {
	choices := make([]semantic.GenerateChoice, len(result.Choices))
	copy(choices, result.Choices)
	for index := range choices {
		content, err := e.processContent(policyID, "outbound", choices[index].Message.Content)
		if err != nil {
			return result, err
		}
		choices[index].Message.Content = content
	}
	result.Choices = choices
	return result, nil
}

// processContent copies before it rewrites. The caller's slice is the request
// that was routed and, on a retry, the request that will be routed again; a
// traversal that edited it in place would hand the second attempt content the
// first one's policy had already transformed.
func (e *Engine) processContent(policyID, scope string, parts []semantic.Content) ([]semantic.Content, error) {
	if len(parts) == 0 {
		return parts, nil
	}
	result := make([]semantic.Content, len(parts))
	copy(result, parts)
	for index := range result {
		part := &result[index]
		switch part.Kind {
		case semantic.ContentText, semantic.ContentToolResult, semantic.ContentReasoning:
			text, err := e.ProcessText(policyID, scope, part.Text)
			if err != nil {
				return parts, err
			}
			citations, err := e.processCitations(policyID, scope, part.Citations, text != part.Text)
			if err != nil {
				return parts, err
			}
			part.Text, part.Citations = text, citations
		case semantic.ContentInputImage:
			// The address is caller-supplied text and was inside the content the
			// Chat pass rewrote. A data URL carries the picture and nothing a rule
			// matches; a remote one can carry a token in a query string.
			url, err := e.ProcessText(policyID, scope, part.URL)
			if err != nil {
				return parts, err
			}
			part.URL = url
		case semantic.ContentToolCall:
			arguments, err := e.processToolArguments(policyID, scope, part.Arguments)
			if err != nil {
				return parts, err
			}
			part.Arguments = arguments
		case semantic.ContentProviderToolCall:
			// The query is the model's own words, drawn from the conversation it
			// was given, so it carries whatever that conversation carried. It
			// reaches the caller as action.query — a string no other pass on this
			// path rewrites, which is the same shape the image address above is
			// guarded for.
			text, err := e.ProcessText(policyID, scope, part.Text)
			if err != nil {
				return parts, err
			}
			part.Text = text
		default:
			// A kind added to the vocabulary without a case here would reach the
			// caller with neither the Project policy nor the mandatory baseline
			// applied, on the one traversal whose purpose is to catch a secret in
			// provider output. Silence is the fail-open; refusing is the only
			// honest reading of "this pass does not know what that part holds".
			return parts, fmt.Errorf("redaction cannot traverse content kind %q", part.Kind)
		}
		if err := e.validateMandatory(part.CallID); err != nil {
			return parts, err
		}
		if err := e.validateMandatory(part.Name); err != nil {
			return parts, err
		}
	}
	return result, nil
}

// processCitations rewrites the sources attributed to a span of text.
//
// The URL and the title are provider-supplied strings that reach the caller
// untouched by every other pass on this path; a citation URL carries a token in
// its query string exactly as an image address can.
//
// The offsets are the harder half. Redaction changes the length of the text it
// rewrites and leaves no diff to map an old span onto the new one. A stale span
// is not merely out of range: when the replacement is longer than what it
// replaced, the span still validates and now covers different words, so a source
// ends up attributed to a sentence that never cited it. When the text changed at
// all, every span therefore collapses to zero length — which is what the inbound
// decoder already does with a span it cannot place. The source is still
// reported; the false precision is not.
func (e *Engine) processCitations(
	policyID, scope string,
	citations []semantic.Citation,
	textChanged bool,
) ([]semantic.Citation, error) {
	if len(citations) == 0 {
		return citations, nil
	}
	// The caller's slice is shared with the message that was routed, and on a
	// retry with the message that will be routed again. Copying the header is not
	// enough for a member the traversal edits.
	result := make([]semantic.Citation, len(citations))
	copy(result, citations)
	for index := range result {
		citation := &result[index]
		url, err := e.ProcessText(policyID, scope, citation.URL)
		if err != nil {
			return citations, err
		}
		title, err := e.ProcessText(policyID, scope, citation.Title)
		if err != nil {
			return citations, err
		}
		citation.URL, citation.Title = url, title
		if textChanged {
			citation.StartIndex, citation.EndIndex = 0, 0
		}
	}
	return result, nil
}

func (e *Engine) ProcessInboundEmbedding(
	policyID string,
	request openaiapi.EmbeddingRequest,
) (openaiapi.EmbeddingRequest, error) {
	var err error
	request.Input, err = e.processRaw(policyID, "inbound", request.Input)
	return request, err
}

func (e *Engine) ProcessText(policyID, scope, value string) (string, error) {
	if scope != "inbound" && scope != "outbound" {
		return "", errors.New("redaction scope is invalid")
	}
	encoded, _ := json.Marshal(value)
	processed, err := e.processRaw(policyID, scope, encoded)
	if err != nil {
		return "", err
	}
	var result string
	if err := json.Unmarshal(processed, &result); err != nil {
		return "", err
	}
	return result, nil
}

func (e *Engine) ProcessJSON(policyID, scope string, value json.RawMessage) (json.RawMessage, error) {
	return e.processRaw(policyID, scope, value)
}

// ProcessOutboundChat is the outbound pass for the streaming path, which is
// still Chat-shaped: a Stream rewrites Chat chunks, so its unary twin has to
// speak the same shape to be comparable — TestStreamMatchesUnaryRedaction is
// that comparison and it is the reason both exist.
//
// The unary generate path does not use it; that side reads
// ProcessOutboundGenerateResult and is measured on the semantic result. The two
// share every transform below them, so the split is in what they walk rather
// than in what the policy does.
func (e *Engine) ProcessOutboundChat(
	policyID string,
	response openaiapi.ChatCompletionResponse,
) (openaiapi.ChatCompletionResponse, error) {
	for choiceIndex := range response.Choices {
		choice := &response.Choices[choiceIndex]
		for _, message := range []*openaiapi.Message{choice.Message, choice.Delta} {
			if message == nil {
				continue
			}
			var err error
			message.Content, err = e.processRaw(policyID, "outbound", message.Content)
			if err != nil {
				return response, err
			}
			for callIndex := range message.ToolCalls {
				arguments, err := e.processToolArguments(
					policyID, "outbound", message.ToolCalls[callIndex].Function.Arguments,
				)
				if err != nil {
					return response, err
				}
				message.ToolCalls[callIndex].Function.Arguments = arguments
			}
		}
	}
	return response, nil
}

// SanitizeOutboundChat keeps the mandatory secret baseline available to callers
// without an assigned Project policy.
func (e *Engine) ValidateInboundEmbedding(request openaiapi.EmbeddingRequest) error {
	_, err := e.ProcessInboundEmbedding("", request)
	return err
}

func (e *Engine) SanitizeOutboundChat(response openaiapi.ChatCompletionResponse) openaiapi.ChatCompletionResponse {
	value, err := e.ProcessOutboundChat("", response)
	if err != nil {
		for choiceIndex := range value.Choices {
			for _, message := range []*openaiapi.Message{
				value.Choices[choiceIndex].Message,
				value.Choices[choiceIndex].Delta,
			} {
				if message == nil {
					continue
				}
				message.Content = openaiapi.TextContent("[REDACTED]")
				for callIndex := range message.ToolCalls {
					message.ToolCalls[callIndex].Function.Arguments = "[REDACTED]"
				}
			}
		}
	}
	return value
}

func (e *Engine) processRaw(policyID, scope string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	value, err := decodeJSONValue(raw)
	if err != nil {
		transformed, err := e.processString(policyID, scope, string(raw))
		return json.RawMessage(transformed), err
	}
	transformed, err := e.transformValue(policyID, scope, value)
	if err != nil {
		return raw, err
	}
	encoded, err := json.Marshal(transformed)
	if err != nil {
		return raw, err
	}
	return encoded, nil
}

// decodeJSONValue parses with UseNumber so a number survives the round trip as
// its original literal. Without it every number became a float64, which both
// corrupted long digit strings — the exact shape a card or account number takes
// — and made the re-marshalled document differ from the input for reasons that
// have nothing to do with the policy.
func decodeJSONValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("payload contains multiple JSON values")
	}
	return value, nil
}

// transformValue rewrites string values, and inspects everything else a secret
// can hide in.
//
// Member names and numbers are checked but never rewritten. Renaming a member
// changes the document's shape rather than its content, and replacing a number
// with a masked string changes its type; either would hand the caller a document
// that no longer means what it did. Reporting the match instead keeps the
// inspected surface equal to the accepted surface — the property the native
// paths rely on — without a rewrite that silently breaks the schema.
func (e *Engine) transformValue(policyID, scope string, value any) (any, error) {
	switch typed := value.(type) {
	case string:
		return e.processString(policyID, scope, typed)
	case json.Number:
		if err := e.inspectString(policyID, scope, typed.String()); err != nil {
			return value, err
		}
	case []any:
		for index := range typed {
			next, err := e.transformValue(policyID, scope, typed[index])
			if err != nil {
				return value, err
			}
			typed[index] = next
		}
	case map[string]any:
		for key := range typed {
			if err := e.inspectString(policyID, scope, key); err != nil {
				return value, err
			}
			next, err := e.transformValue(policyID, scope, typed[key])
			if err != nil {
				return value, err
			}
			typed[key] = next
		}
	}
	return value, nil
}

func (e *Engine) processString(policyID, scope, value string) (string, error) {
	if scope == "inbound" {
		if err := e.validateMandatory(value); err != nil {
			return value, err
		}
	} else {
		var err error
		value, err = e.sanitizeMandatoryOutbound(value)
		if err != nil {
			return value, err
		}
	}
	policy, ok := e.policy(policyID)
	if !ok {
		// An empty ID means the Project has no policy, which is a decision. A
		// named policy that is not in the live snapshot is not: returning the
		// value untouched runs zero of that Project's rules while reporting
		// success. The Gateway refuses such a Project up front; this is the
		// same answer one layer down, for the window in which the snapshot is
		// replaced mid-request.
		if policyID == "" {
			return value, nil
		}
		return value, ErrPolicyUnavailable
	}
	for _, rule := range policy.rules {
		if !hasScope(rule.rule, scope) || !rule.matches(value) {
			continue
		}
		e.recordHit(policyID, rule.rule.ID)
		switch rule.rule.Action {
		case "detect_only":
		case "reject":
			return value, &MatchError{
				RuleID: rule.rule.ID, Category: category(rule.rule), Scope: scope,
			}
		case "replace":
			value = rule.replaceAll(value, rule.rule.Replacement, nil)
		case "mask":
			value = rule.replaceAll(value, "", func(match string) string {
				return masked(category(rule.rule), match)
			})
		}
	}
	return value, nil
}

func (e *Engine) processToolArguments(policyID, scope, arguments string) (string, error) {
	if arguments == "" {
		return arguments, nil
	}
	var value any
	if err := json.Unmarshal([]byte(arguments), &value); err != nil {
		return e.processToolFragment(policyID, scope, arguments)
	}
	transformed, err := e.transformValue(policyID, scope, value)
	if err != nil {
		return arguments, err
	}
	encoded, err := json.Marshal(transformed)
	if err != nil {
		return arguments, err
	}
	return string(encoded), nil
}

// Streaming tool arguments are incomplete JSON fragments. detect/reject are
// safe; mask/replace must fail closed because transforming arbitrary syntax
// before the complete JSON value is available can corrupt its structure.
func (e *Engine) processToolFragment(policyID, scope, value string) (string, error) {
	if scope == "inbound" {
		if err := e.validateMandatory(value); err != nil {
			return value, err
		}
	} else {
		var err error
		value, err = e.sanitizeMandatoryOutbound(value)
		if err != nil {
			return value, err
		}
	}
	policy, ok := e.policy(policyID)
	if !ok {
		// An empty ID means the Project has no policy, which is a decision. A
		// named policy that is not in the live snapshot is not: returning the
		// value untouched runs zero of that Project's rules while reporting
		// success. The Gateway refuses such a Project up front; this is the
		// same answer one layer down, for the window in which the snapshot is
		// replaced mid-request.
		if policyID == "" {
			return value, nil
		}
		return value, ErrPolicyUnavailable
	}
	for _, rule := range policy.rules {
		if !hasScope(rule.rule, scope) || !rule.matches(value) {
			continue
		}
		e.recordHit(policyID, rule.rule.ID)
		if rule.rule.Action == "detect_only" {
			continue
		}
		return value, &MatchError{
			RuleID: rule.rule.ID, Category: category(rule.rule), Scope: scope,
		}
	}
	return value, nil
}

func (e *Engine) policy(id string) (compiledPolicy, bool) {
	if id == "" {
		return compiledPolicy{}, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	policy, exists := e.policies[id]
	return policy, exists
}

func (e *Engine) recordHit(policyID, ruleID string) {
	e.mu.Lock()
	e.hits[policyID+":"+ruleID]++
	e.mu.Unlock()
}

func (e *Engine) validateMandatory(value string) error {
	for _, rule := range e.mandatory {
		if rule.matches(value) {
			return ErrSecretDetected
		}
	}
	return nil
}

func (e *Engine) sanitizeMandatoryOutbound(value string) (string, error) {
	for _, rule := range e.mandatory {
		if rule.rule.Builtin == "private_key" && rule.matches(value) {
			return value, &MatchError{
				RuleID: rule.rule.ID, Category: rule.rule.Builtin, Scope: "outbound",
			}
		}
		value = rule.replaceAll(value, "[REDACTED]", nil)
	}
	return value, nil
}

func compileRule(rule domain.RedactionRule) (compiledRule, error) {
	var expression string
	switch rule.Kind {
	case "builtin":
		var exists bool
		expression, exists = builtinPatterns[rule.Builtin]
		if !exists {
			return compiledRule{}, errors.New("unknown builtin category")
		}
	case "regex":
		expression = rule.Pattern
	case "dictionary":
		values := make([]string, 0, len(rule.Dictionary))
		maxBytes := 0
		for _, value := range rule.Dictionary {
			if value == "" || len(value) > 256 {
				return compiledRule{}, errors.New("dictionary entries must contain 1 to 256 bytes")
			}
			values = append(values, regexp.QuoteMeta(value))
			maxBytes = max(maxBytes, len(value))
		}
		slices.SortFunc(values, func(left, right string) int { return len(right) - len(left) })
		expression = "(?:" + strings.Join(values, "|") + ")"
		rule.ComputedMaxMatchBytes = maxBytes
	}
	pattern, err := regexp.Compile(expression)
	if err != nil {
		return compiledRule{}, err
	}
	if rule.Kind != "dictionary" {
		parsed, err := syntax.Parse(expression, syntax.Perl)
		if err != nil {
			return compiledRule{}, err
		}
		rule.ComputedMaxMatchBytes = maxMatchBytes(parsed)
	}
	compiled := compiledRule{rule: rule, pattern: pattern}
	switch rule.Builtin {
	case "bank_card_candidate":
		compiled.validator = validBankCard
	case "china_id":
		compiled.validator = validChinaID
	}
	return compiled, nil
}

func (r compiledRule) matches(value string) bool {
	return len(r.validLocations(value)) > 0
}

func (r compiledRule) validLocations(value string) [][]int {
	locations := r.pattern.FindAllStringSubmatchIndex(value, -1)
	valid := locations[:0]
	for _, location := range locations {
		if r.semanticBoundary(value, location[0], location[1]) &&
			(r.validator == nil || r.validator(value[location[0]:location[1]])) {
			valid = append(valid, location)
		}
	}
	return valid
}

func (r compiledRule) semanticBoundary(value string, start, end int) bool {
	switch r.rule.Builtin {
	case "china_phone", "china_id", "bank_card_candidate":
		return (start == 0 || value[start-1] < '0' || value[start-1] > '9') &&
			(end == len(value) || value[end] < '0' || value[end] > '9')
	default:
		return true
	}
}

func (r compiledRule) replaceAll(
	value string,
	replacement string,
	replacer func(string) string,
) string {
	locations := r.validLocations(value)
	if len(locations) == 0 {
		return value
	}
	result := make([]byte, 0, len(value))
	previous := 0
	for _, location := range locations {
		result = append(result, value[previous:location[0]]...)
		if replacer != nil {
			result = append(result, replacer(value[location[0]:location[1]])...)
		} else {
			result = r.pattern.ExpandString(result, replacement, value, location)
		}
		previous = location[1]
	}
	return string(append(result, value[previous:]...))
}

func validBankCard(value string) bool {
	digits := make([]byte, 0, len(value))
	for index := range len(value) {
		switch value[index] {
		case ' ', '-':
		default:
			if value[index] < '0' || value[index] > '9' {
				return false
			}
			digits = append(digits, value[index])
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	parity := len(digits) % 2
	for index, digit := range digits {
		number := int(digit - '0')
		if index%2 == parity {
			number *= 2
			if number > 9 {
				number -= 9
			}
		}
		sum += number
	}
	return sum%10 == 0
}

func validChinaID(value string) bool {
	if len(value) != 18 {
		return false
	}
	weights := [...]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for index, weight := range weights {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
		sum += int(value[index]-'0') * weight
	}
	if _, err := time.Parse("20060102", value[6:14]); err != nil {
		return false
	}
	expected := "10X98765432"[sum%11]
	actual := value[17]
	if actual == 'x' {
		actual = 'X'
	}
	return actual == expected
}

func maxMatchBytes(expression *syntax.Regexp) int {
	switch expression.Op {
	case syntax.OpNoMatch, syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return 0
	case syntax.OpLiteral:
		total := 0
		for _, value := range expression.Rune {
			total += utf8.RuneLen(value)
		}
		return total
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return utf8.UTFMax
	case syntax.OpCapture:
		return maxMatchBytes(expression.Sub[0])
	case syntax.OpConcat:
		total := 0
		for _, child := range expression.Sub {
			width := maxMatchBytes(child)
			if width < 0 || total > maxBoundedMatchBytes-width {
				return -1
			}
			total += width
		}
		return total
	case syntax.OpAlternate:
		result := 0
		for _, child := range expression.Sub {
			width := maxMatchBytes(child)
			if width < 0 {
				return -1
			}
			result = max(result, width)
		}
		return result
	case syntax.OpQuest:
		return maxMatchBytes(expression.Sub[0])
	case syntax.OpStar, syntax.OpPlus:
		return -1
	case syntax.OpRepeat:
		if expression.Max < 0 {
			return -1
		}
		width := maxMatchBytes(expression.Sub[0])
		if width < 0 || width > maxBoundedMatchBytes/max(expression.Max, 1) {
			return -1
		}
		return width * expression.Max
	default:
		return -1
	}
}

func hasScope(rule domain.RedactionRule, scope string) bool {
	return slices.Contains(rule.Scopes, scope)
}

func category(rule domain.RedactionRule) string {
	if rule.Builtin != "" {
		return rule.Builtin
	}
	return rule.Name
}

func masked(category, value string) string {
	switch category {
	case "china_phone", "bank_card_candidate":
		digits := make([]rune, 0, len(value))
		for _, char := range value {
			if char >= '0' && char <= '9' {
				digits = append(digits, char)
			}
		}
		if len(digits) >= 4 {
			return "••••" + string(digits[len(digits)-4:])
		}
	case "email":
		if at := strings.LastIndexByte(value, '@'); at > 0 {
			return "•••" + value[at:]
		}
	}
	return "[MASKED]"
}
