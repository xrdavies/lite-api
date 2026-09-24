package app

import (
	"encoding/json"
	"math/big"
	"strings"
	"time"
)

func validateGrokHostedSearch(tool map[string]json.RawMessage) error {
	kind := credentialString(tool, "type")
	allowed, excluded, limit := "allowed_domains", "excluded_domains", 5
	if kind == "x_search" {
		allowed, excluded, limit = "allowed_x_handles", "excluded_x_handles", 20
	}
	lists := map[string][]string{}
	for name, raw := range tool {
		switch name {
		case "type":
		case allowed, excluded:
			var values []string
			if string(raw) == "null" || json.Unmarshal(raw, &values) != nil || len(values) > limit {
				return bad("invalid hosted search filter")
			}
			for _, value := range values {
				if strings.TrimSpace(value) == "" || len(value) > 253 || strings.ContainsAny(value, "\x00\r\n") {
					return bad("invalid hosted search filter value")
				}
			}
			lists[name] = values
		case "enable_image_understanding", "enable_video_understanding", "enable_image_search":
			if name == "enable_video_understanding" && kind != "x_search" || name == "enable_image_search" && kind != "web_search" || string(raw) != "true" && string(raw) != "false" {
				return bad("invalid hosted search option")
			}
		case "from_date", "to_date":
			if _, err := time.Parse("2006-01-02", credentialString(tool, name)); kind != "x_search" || err != nil {
				return bad("invalid hosted search date")
			}
		default:
			return bad("unsupported hosted search option")
		}
	}
	if len(lists[allowed]) > 0 && len(lists[excluded]) > 0 {
		return bad("use allowed or excluded search filters, not both")
	}
	if from, to := credentialString(tool, "from_date"), credentialString(tool, "to_date"); from != "" && to != "" && from > to {
		return bad("hosted search dates are reversed")
	}
	return nil
}

// Item completion events and the terminal output repeat the same calls. Stable
// IDs deduplicate events; cumulative output/usage replace smaller event counts.
// Only native calls are billable: a client function named web_search is not one.
type grokHostedSearchMeter struct {
	seen                map[string]bool
	done, output, usage [2]int64
}

func (m *grokHostedSearchMeter) count() int64 {
	var total int64
	for i := range m.done {
		total += max(m.done[i], m.output[i], m.usage[i])
	}
	return total
}

func (m *grokHostedSearchMeter) observe(raw []byte) error {
	var event struct {
		Type, Status   string
		Item, Response json.RawMessage
		Output         []json.RawMessage
		Usage          struct {
			Details map[string]json.RawMessage `json:"server_side_tool_usage_details"`
		}
	}
	invalid := func() error { return &apiError{502, "invalid upstream hosted search usage"} }
	if json.Unmarshal(raw, &event) != nil {
		return invalid()
	}
	if event.Response != nil && string(event.Response) != "null" {
		// Prefer the canonical response envelope, never count duplicate outer output.
		var nested map[string]json.RawMessage
		if json.Unmarshal(event.Response, &nested) != nil || nested["response"] != nil {
			return invalid()
		}
		return m.observe(event.Response)
	}
	items := event.Output
	done := event.Type == "response.output_item.done"
	if done {
		items = []json.RawMessage{event.Item}
	} else if event.Type != "" || event.Status != "completed" && event.Status != "incomplete" && event.Status != "failed" && event.Status != "cancelled" {
		return nil
	}
	var counts [2]int64
	seen := map[string]bool{}
	if done {
		if m.seen == nil {
			m.seen = map[string]bool{}
		}
		seen = m.seen
	}
	for _, raw := range items {
		var item struct {
			Type, Status, ID string
			CallID           string `json:"call_id"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return invalid()
		}
		i := 0
		switch item.Type {
		case "web_search_call":
		case "x_search_call":
			i = 1
		default:
			continue
		}
		if item.Status != "" && item.Status != "completed" && item.Status != "succeeded" {
			continue
		}
		id := item.ID
		if id == "" {
			id = item.CallID
		}
		if len(id) > 256 {
			return invalid()
		}
		if id == "" {
			counts[i]++
		} else {
			key := item.Type + ":" + id
			if !seen[key] {
				seen[key] = true
				counts[i]++
			}
		}
		if counts[0]+counts[1]+int64(len(m.seen)) > 20000 {
			return invalid()
		}
	}
	for i, name := range []string{"web_search_calls", "x_search_calls"} {
		if done {
			m.done[i] += counts[i]
		} else {
			m.output[i] = max(m.output[i], counts[i])
		}
		if raw := event.Usage.Details[name]; raw != nil {
			var calls int64
			if json.Unmarshal(raw, &calls) != nil || string(raw) == "null" || calls < 0 || calls > 10000 {
				return invalid()
			}
			m.usage[i] = max(m.usage[i], calls)
		}
	}
	if m.count() > 10000 {
		return invalid()
	}
	return nil
}

func addGrokSearchCost(cost *priceCost, group gatewayGroup, calls int64) error {
	if calls < 0 || calls > 10000 {
		return bad("invalid search call count")
	}
	search, err := group.searchCost("web_search")
	if err != nil {
		return err
	}
	total := new(big.Rat).Mul(search.totalValue, big.NewRat(calls, 1))
	cost.totalValue.Add(cost.totalValue, total)
	actual := new(big.Rat).Mul(cost.totalValue, rat(group.Rate))
	cost.Total, cost.Actual, cost.Debit = cost.totalValue.FloatString(10), actual.FloatString(10), actual.FloatString(8)
	return nil
}
