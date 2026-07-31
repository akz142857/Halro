package domain

import (
	"errors"
	"strings"
	"time"
)

type RedactionRule struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	Kind                  string   `json:"kind"`
	Builtin               string   `json:"builtin,omitempty"`
	Pattern               string   `json:"pattern,omitempty"`
	Dictionary            []string `json:"dictionary,omitempty"`
	Scopes                []string `json:"scopes"`
	Action                string   `json:"action"`
	Replacement           string   `json:"replacement,omitempty"`
	Enabled               bool     `json:"enabled"`
	Priority              int      `json:"priority"`
	ComputedMaxMatchBytes int      `json:"computed_max_match_bytes"`
}

type RedactionPolicy struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Mode      string          `json:"mode"`
	Rules     []RedactionRule `json:"rules"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Revision  uint64          `json:"revision"`
	DeletedAt *time.Time      `json:"deleted_at,omitempty"`
}

func (p *RedactionPolicy) GetRevision() uint64      { return p.Revision }
func (p *RedactionPolicy) SetRevision(value uint64) { p.Revision = value }

func (p RedactionPolicy) Validate() error {
	var problems []error
	if p.ID == "" {
		problems = append(problems, errors.New("redaction policy id is required"))
	}
	if strings.TrimSpace(p.Name) == "" {
		problems = append(problems, errors.New("redaction policy name is required"))
	}
	switch p.Mode {
	case "strict", "bounded_stream", "detect_only_stream":
	default:
		problems = append(problems, errors.New(
			"redaction policy mode must be strict, bounded_stream, or detect_only_stream",
		))
	}
	if len(p.Rules) > 128 {
		problems = append(problems, errors.New("redaction policy cannot contain more than 128 rules"))
	}
	ids := make(map[string]struct{}, len(p.Rules))
	for index, rule := range p.Rules {
		if err := rule.Validate(); err != nil {
			problems = append(problems, errors.New("redaction rule "+err.Error()))
		}
		if _, exists := ids[rule.ID]; rule.ID != "" && exists {
			problems = append(problems, errors.New("redaction rule ids must be unique"))
		}
		ids[rule.ID] = struct{}{}
		if rule.Priority < 0 || rule.Priority > 10_000 {
			problems = append(problems, errors.New("redaction rule priority is out of range"))
		}
		if len(rule.Pattern) > 2048 {
			problems = append(problems, errors.New("redaction rule pattern is too large"))
		}
		if len(rule.Dictionary) > 1024 {
			problems = append(problems, errors.New("redaction dictionary is too large"))
		}
		if index > 128 {
			break
		}
	}
	return errors.Join(problems...)
}

func (r RedactionRule) Validate() error {
	var problems []error
	if r.ID == "" {
		problems = append(problems, errors.New("id is required"))
	}
	if strings.TrimSpace(r.Name) == "" {
		problems = append(problems, errors.New("name is required"))
	}
	switch r.Kind {
	case "builtin":
		if r.Builtin == "" {
			problems = append(problems, errors.New("builtin category is required"))
		}
	case "regex":
		if r.Pattern == "" {
			problems = append(problems, errors.New("pattern is required"))
		}
	case "dictionary":
		if len(r.Dictionary) == 0 {
			problems = append(problems, errors.New("dictionary must not be empty"))
		}
	default:
		problems = append(problems, errors.New("kind must be builtin, regex, or dictionary"))
	}
	if len(r.Scopes) == 0 {
		problems = append(problems, errors.New("scope is required"))
	}
	seen := make(map[string]struct{}, len(r.Scopes))
	for _, scope := range r.Scopes {
		if scope != "inbound" && scope != "outbound" {
			problems = append(problems, errors.New("scope must be inbound or outbound"))
		}
		if _, exists := seen[scope]; exists {
			problems = append(problems, errors.New("scopes must be unique"))
		}
		seen[scope] = struct{}{}
	}
	switch r.Action {
	case "detect_only", "mask", "replace", "reject":
	default:
		problems = append(problems, errors.New(
			"action must be detect_only, mask, replace, or reject",
		))
	}
	if r.Action == "replace" && strings.TrimSpace(r.Replacement) == "" {
		problems = append(problems, errors.New("replacement is required"))
	}
	if len(r.Replacement) > 256 {
		problems = append(problems, errors.New("replacement is too large"))
	}
	return errors.Join(problems...)
}
