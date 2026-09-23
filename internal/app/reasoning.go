package app

import (
	"encoding/json"
	"slices"
	"strings"
)

type effortMapping struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Match string `json:"match_type,omitempty"`
	Model string `json:"model,omitempty"`
}

type reasoningPolicy struct {
	MaxEffort string          `json:"max_reasoning_effort"`
	OverLimit string          `json:"max_reasoning_effort_over_limit"`
	Mappings  []effortMapping `json:"reasoning_effort_mappings"`
}

func canonicalEffort(value string) string {
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	if value == "extrahigh" {
		value = "xhigh"
	}
	if effortRank(value) > 0 {
		return value
	}
	return ""
}

func effortRank(value string) int {
	return slices.Index([]string{"minimal", "low", "medium", "high", "xhigh", "max"}, value) + 1
}

func (in *groupInput) validateReasoning(platform string) error {
	allowed := func(value string) bool {
		return effortRank(value) > 0 && (platform == "openai" || platform == "composite" || platform == "anthropic" && value != "minimal")
	}
	if in.MaxEffort != nil {
		value := canonicalEffort(*in.MaxEffort)
		if strings.TrimSpace(*in.MaxEffort) != "" && !allowed(value) {
			return bad("unsupported maximum reasoning effort for this platform")
		}
		*in.MaxEffort = value
	}
	if in.OverLimit != nil {
		value := strings.ToLower(strings.TrimSpace(*in.OverLimit))
		if value == "" {
			value = "downgrade"
		}
		if value != "downgrade" && value != "deny" || value == "deny" && !allowed("low") {
			return bad("invalid reasoning effort over-limit action")
		}
		*in.OverLimit = value
	}
	if in.EffortMappings != nil {
		if len(*in.EffortMappings) > 64 {
			return bad("too many reasoning effort mappings")
		}
		seen := map[string]bool{}
		for i := range *in.EffortMappings {
			m := &(*in.EffortMappings)[i]
			from, to := canonicalEffort(m.From), canonicalEffort(m.To)
			if strings.EqualFold(strings.TrimSpace(m.From), "none") {
				from = "none"
			}
			if strings.EqualFold(strings.TrimSpace(m.To), "deny") {
				to = "deny"
			}
			if !allowed("low") || from != "none" && !allowed(from) || to != "deny" && !allowed(to) {
				return bad("invalid reasoning effort mapping values")
			}
			m.From, m.To = from, to
			m.Model, m.Match = strings.TrimSpace(m.Model), strings.ToLower(strings.TrimSpace(m.Match))
			if len(m.Model) > 200 || strings.ContainsAny(m.Model, "\r\n\x00") {
				return bad("invalid reasoning effort model scope")
			}
			if m.Model == "" {
				m.Match = ""
			} else if m.Match == "" {
				m.Match = "exact"
			}
			if m.Match != "" && m.Match != "exact" && m.Match != "prefix" && m.Match != "suffix" {
				return bad("invalid reasoning effort match type")
			}
			key := m.From + "\x00" + m.Match + "\x00" + strings.ToLower(m.Model)
			if seen[key] {
				return bad("duplicate reasoning effort mapping")
			}
			seen[key] = true
		}
	}
	return nil
}

func (p reasoningPolicy) mappedEffort(value, model string) (string, bool) {
	from := canonicalEffort(value)
	if strings.EqualFold(strings.TrimSpace(value), "none") {
		from = "none"
	}
	best, strength, length := -1, 0, 0
	for i, m := range p.Mappings {
		if m.From != from {
			continue
		}
		scope, request := strings.ToLower(m.Model), strings.ToLower(strings.TrimSpace(model))
		rank := 0
		switch m.Match {
		case "":
			rank = 1
		case "exact":
			if request == scope {
				rank = 3
			}
		case "prefix":
			if strings.HasPrefix(request, scope) {
				rank = 2
			}
		case "suffix":
			if strings.HasSuffix(request, scope) {
				rank = 2
			}
		}
		if rank > strength || rank > 0 && rank == strength && len(scope) > length {
			best, strength, length = i, rank, len(scope)
		}
	}
	if best >= 0 {
		return p.Mappings[best].To, true
	}
	return value, false
}

// Apply once to explicit fields, before model rewriting; never synthesize effort.
func (p reasoningPolicy) apply(body map[string]json.RawMessage, model, platform string) error {
	if platform != "openai" && platform != "anthropic" || p.MaxEffort == "" && len(p.Mappings) == 0 {
		return nil
	}
	max := p.MaxEffort
	if platform == "anthropic" && max == "minimal" {
		max = "low"
	}
	for _, path := range [][2]string{{"reasoning", "effort"}, {"reasoning_effort", ""}, {"output_config", "effort"}} {
		object, field := body, path[0]
		if path[1] != "" {
			var nested map[string]json.RawMessage
			if json.Unmarshal(body[path[0]], &nested) != nil || nested == nil {
				continue
			}
			object, field = nested, path[1]
		}
		var value string
		if json.Unmarshal(object[field], &value) != nil || strings.TrimSpace(value) == "" {
			continue
		}
		effective, mapped := p.mappedEffort(strings.TrimSpace(value), model)
		if mapped && effective == "deny" {
			return &apiError{403, "reasoning effort is denied by group policy"}
		}
		if canonical := canonicalEffort(effective); canonical != "" {
			effective = canonical
			if mapped && platform == "anthropic" && effective == "minimal" {
				effective = "low"
			}
			if effortRank(max) > 0 && effortRank(effective) > effortRank(max) {
				if p.OverLimit == "deny" {
					return &apiError{403, "reasoning effort exceeds the group limit"}
				}
				effective = max
			}
		}
		if effective != value {
			object[field], _ = json.Marshal(effective)
			if path[1] != "" {
				body[path[0]], _ = json.Marshal(object)
			}
		}
	}
	return nil
}

func requestEffort(body map[string]json.RawMessage, protocol string) (string, error) {
	field, nested := "reasoning_effort", ""
	if protocol == "responses" {
		field, nested = "reasoning", "effort"
	}
	if protocol == "anthropic" {
		field, nested = "output_config", "effort"
	}
	raw := body[field]
	if nested != "" && len(raw) > 0 && string(raw) != "null" {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return "", bad("invalid reasoning configuration")
		}
		raw = object[nested]
	}
	var value string
	if len(raw) > 0 && (json.Unmarshal(raw, &value) != nil || len(value) > 20) {
		return "", bad("invalid reasoning effort")
	}
	if protocol == "responses" && strings.TrimSpace(value) == "" {
		return requestEffort(body, "chat_completions")
	}
	if canonical := canonicalEffort(value); canonical != "" {
		return canonical, nil
	}
	return strings.TrimSpace(value), nil
}

func requestedEffort(body map[string]json.RawMessage, model string) *string {
	for _, protocol := range []string{"responses", "chat_completions", "anthropic"} {
		value, _ := requestEffort(body, protocol)
		if value != "" {
			if canonical := canonicalEffort(value); canonical != "" {
				return &canonical
			}
			return nil
		}
	}
	parts := strings.FieldsFunc(model, func(r rune) bool { return r == '/' || r == '-' || r == '_' || r == ' ' })
	if len(parts) > 0 {
		if canonical := canonicalEffort(parts[len(parts)-1]); canonical != "" {
			return &canonical
		}
	}
	return nil
}
