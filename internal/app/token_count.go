package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// Local counts are a compatibility estimate, never a source of billable usage.
// Media bytes are not tokenized or fetched; only their descriptors contribute.
func estimateInputTokens(raw []byte) (int, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return 0, bad("invalid token count input")
	}
	model := strings.ToLower(credentialString(body, "model"))
	encoding := tokenizer.O200kBase
	if strings.HasPrefix(model, "gpt-3.5") || strings.HasPrefix(model, "text-embedding-") || strings.HasPrefix(model, "gpt-4") && !strings.HasPrefix(model, "gpt-4o") && !strings.HasPrefix(model, "gpt-4.1") {
		encoding = tokenizer.Cl100kBase
	}
	codec, err := tokenizer.Get(encoding)
	if err != nil {
		return 0, err
	}
	total := 0
	count := func(text string) error {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		// ponytail: split long strings at UTF-8 boundaries to bound quadratic BPE
		// work. Chunk boundaries can alter the estimate; exact counts need upstream.
		for len(text) > 0 {
			end := min(len(text), 4096)
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end--
			}
			n, err := codec.Count(text[:end])
			if err != nil {
				return err
			}
			total += n
			text = text[end:]
		}
		return nil
	}
	compact := func(raw json.RawMessage) error {
		var out bytes.Buffer
		if json.Compact(&out, raw) != nil {
			return bad("invalid token count content")
		}
		return count(out.String())
	}
	content := func(raw json.RawMessage) error {
		if len(raw) == 0 || string(raw) == "null" {
			return nil
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return count(text)
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(raw, &parts) != nil {
			return compact(raw)
		}
		for _, part := range parts {
			if part == nil {
				return bad("invalid token count content part")
			}
			total++
			kind := credentialString(part, "type")
			switch kind {
			case "input_text", "output_text", "text":
				text = credentialString(part, "text")
			case "input_image":
				text = credentialString(part, "image_url")
				if strings.HasPrefix(strings.ToLower(text), "data:") {
					text, _, _ = strings.Cut(text, ",")
				}
			default:
				text = kind
			}
			if err := count(text); err != nil {
				return err
			}
		}
		return nil
	}
	if err = count(credentialString(body, "instructions")); err != nil {
		return 0, err
	}
	var items []map[string]json.RawMessage
	var plain string
	if json.Unmarshal(body["input"], &plain) == nil {
		err = count(plain)
	} else if json.Unmarshal(body["input"], &items) != nil {
		return 0, bad("token count input must be text or an array")
	} else {
		for _, item := range items {
			if item == nil {
				return 0, bad("invalid token count input item")
			}
			total += 3
			for _, key := range []string{"role", "type", "name", "arguments", "call_id", "id"} {
				text := credentialString(item, key)
				if key == "type" && text == "message" {
					continue
				}
				if err = count(text); err != nil {
					return 0, err
				}
			}
			for _, key := range []string{"output", "content"} {
				if err = content(item[key]); err != nil {
					return 0, err
				}
			}
		}
	}
	if err != nil {
		return 0, err
	}
	var tools []json.RawMessage
	if raw := body["tools"]; raw != nil && json.Unmarshal(raw, &tools) != nil {
		return 0, bad("invalid token count tools")
	}
	for _, tool := range tools {
		if err = compact(tool); err != nil {
			return 0, err
		}
	}
	if raw := body["tool_choice"]; raw != nil {
		if err = compact(raw); err != nil {
			return 0, err
		}
	}
	return max(total, 1), nil
}

// Grok counting needs identity and routing permissions, but no supplier account
// or spending eligibility. Composite and client fallback use the resolved target.
func (a *App) grokTokenCount(r *http.Request, g *gatewayIdentity, in textRequest, body map[string]json.RawMessage) (int, bool, error) {
	if in.Protocol != "anthropic" || !in.CountOnly {
		return 0, false, nil
	}
	routing := g.dispatchGroup()
	platform := routing.Platform
	if platform == "composite" {
		config, err := a.loadComposite(r.Context(), routing.ID)
		if err != nil {
			return 0, true, err
		}
		decision := config.resolve(routing.ID, in.Model, in.compositeEndpoint())
		if !decision.Matched {
			return 0, true, &apiError{404, decision.Reason}
		}
		platform = decision.TargetPlatform
	}
	if platform != "grok" {
		return 0, false, nil
	}
	saved, _, err := a.messagesReasoningInput(g, body)
	if err != nil {
		return 0, true, err
	}
	raw, err := messagesCountRequest(body, saved)
	if err != nil {
		return 0, true, err
	}
	n, err := estimateInputTokens(raw)
	if err == nil {
		err = a.gatewayRPM(r.Context(), g)
	}
	return n, true, err
}
