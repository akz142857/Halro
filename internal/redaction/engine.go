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

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
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

func (e *Engine) ProcessInboundChat(
	policyID string,
	request openaiapi.ChatCompletionRequest,
) (openaiapi.ChatCompletionRequest, error) {
	for index := range request.Messages {
		message := &request.Messages[index]
		var err error
		message.Content, err = e.processRaw(policyID, "inbound", message.Content)
		if err != nil {
			return request, err
		}
		if err := e.validateMandatory(message.ToolCallID); err != nil {
			return request, err
		}
		if err := e.validateMandatory(message.Name); err != nil {
			return request, err
		}
		for callIndex := range message.ToolCalls {
			arguments, err := e.processToolArguments(
				policyID, "inbound", message.ToolCalls[callIndex].Function.Arguments,
			)
			if err != nil {
				return request, err
			}
			message.ToolCalls[callIndex].Function.Arguments = arguments
		}
	}
	return request, nil
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

// Compatibility wrappers keep the mandatory secret baseline available to
// callers without an assigned Project policy.
func (e *Engine) ValidateInboundChat(request openaiapi.ChatCompletionRequest) error {
	_, err := e.ProcessInboundChat("", request)
	return err
}

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
	var value any
	if json.Unmarshal(raw, &value) != nil {
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

func (e *Engine) transformValue(policyID, scope string, value any) (any, error) {
	switch typed := value.(type) {
	case string:
		return e.processString(policyID, scope, typed)
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
		return value, nil
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
		return value, nil
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
