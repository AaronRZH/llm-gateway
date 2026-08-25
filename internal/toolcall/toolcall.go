package toolcall

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// OpenAIToolCall mirrors the OpenAI tool_call shape used across the gateway.
type OpenAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function map[string]interface{} `json:"function"`
}

// Result holds the normalized output after processing a possibly mixed
// (text + embedded XML tool calls) content blob.
type Result struct {
	CleanContent string
	ToolCalls    []OpenAIToolCall
}

// Supported open-tag families: their pattern and matching close tag.
type tagFamily struct {
	open  string
	close string
}

var (
	LT_SL = `</`
	GT = `>`
)

var families = []tagFamily{
	{open: `<function=([a-zA-Z0-9_.-]+)>`, close: `</function>`},
	{open: `<tool_name=([a-zA-Z0-9_.-]+)>`, close: `</tool_name>`},
	{open: `<tool_call=([a-zA-Z0-9_.-]+)>`, close: `</tool_call>`},
	{open: `<invoke name=([a-zA-Z0-9_.-]+)>`, close: `</invoke>`},
}

// buildRe matches any supported open tag (no ^ anchor). Capture group 1 = name.
func buildRe() *regexp.Regexp {
	pattern := families[0].open
	for _, f := range families[1:] {
		pattern += "|" + f.open
	}
	return regexp.MustCompile(pattern)
}

// buildSiblingRe matches the start of any open tag (no capture).
func buildSiblingRe() *regexp.Regexp {
	parts := []string{
		`<function=[a-zA-Z0-9_.-]+>`,
		`<tool_name=[a-zA-Z0-9_.-]+>`,
		`<tool_call=[a-zA-Z0-9_.-]+>`,
		`<invoke name=[a-zA-Z0-9_.-]+>`,
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}

// paramRe matches `<parameter=KEY>VALUE</parameter`.
func paramRe() *regexp.Regexp {
	return regexp.MustCompile(`<parameter=([a-zA-Z0-9_.-]+)>([\s\S]*?)</parameter>`)
}

// Normalize scans text for embedded XML tool-call tags and converts each one
// into an OpenAI-shaped tool call, stripping the matching XML from the text.
//
// Supported open-tag spellings: <function=NAME>, <tool_name=NAME>, <tool_call=NAME>.
// Supported payload shapes:
//   1. Inline JSON: {"a":1}
//   2. Nested parameters: <parameter=a>1</parameter>
// Missing close tags are tolerated: the next sibling open tag terminates the block.
func Normalize(text string) Result {
	r := Result{}
	if text == "" {
		return r
	}
	re := buildRe()
	sibRe := buildSiblingRe()
	pRe := paramRe()
	var cleanParts []string
	pos := 0
	searchFrom := 0
	for {
		remaining := text[searchFrom:]
		loc := re.FindStringIndex(remaining)
		if loc == nil {
			break
		}
		absStart := searchFrom + loc[0]
		absEnd := searchFrom + loc[1]
		openTag := remaining[loc[0]:loc[1]]
		var name, closeTag string
		for _, f := range families {
			fr := regexp.MustCompile(f.open)
			if m := fr.FindStringSubmatch(openTag); m != nil {
				name = m[1]
				closeTag = f.close
				break
			}
		}
		if name == "" {
			searchFrom = absEnd
			continue
		}
		cleanParts = append(cleanParts, text[pos:absStart])
		rest := text[absEnd:]
		closeIdx := strings.Index(rest, closeTag)
		if closeIdx < 0 {
			closeIdx = strings.Index(rest, LT_SL + name + GT)
			closeTag = LT_SL + name + GT
		}
		sibIdx := -1
		if sibLoc := sibRe.FindStringIndex(rest); sibLoc != nil {
			sibIdx = sibLoc[0]
		}
		end := -1
		chosen := -1
		if closeIdx >= 0 && (end < 0 || closeIdx < end) {
			end = closeIdx
			chosen = 0
		}
		if sibIdx >= 0 && (end < 0 || sibIdx < end) {
			end = sibIdx
			chosen = 1
		}
		var payload string
		if end >= 0 && chosen == 0 {
			payload = rest[:end]
			pos = absEnd + end + len(closeTag)
		} else if end >= 0 && chosen == 1 {
			payload = rest[:end]
			pos = absEnd
		} else {
			payload = rest
			pos = len(text)
		}
		searchFrom = pos
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		args := extractArgs(payload, pRe)
		argsJSON, _ := json.Marshal(args)
		r.ToolCalls = append(r.ToolCalls, OpenAIToolCall{
			ID:   "call_" + uuid.New().String()[:8],
			Type: "function",
			Function: map[string]interface{}{
				"name":      name,
				"arguments": string(argsJSON),
			},
		})
	}
	cleanParts = append(cleanParts, text[pos:])
	r.CleanContent = strings.TrimSpace(strings.Join(cleanParts, ""))
	return r
}

// extractArgs turns a payload blob into a map[string]interface{} of arguments.
func extractArgs(payload string, pRe *regexp.Regexp) map[string]interface{} {
	params := pRe.FindAllStringSubmatch(payload, -1)
	if len(params) > 0 {
		out := make(map[string]interface{}, len(params))
		for _, p := range params {
			key := strings.TrimSpace(p[1])
			val := p[2]
			var parsed interface{}
			if err := json.Unmarshal([]byte(val), &parsed); err == nil {
				out[key] = parsed
			} else {
				out[key] = val
			}
		}
		return out
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
		if m, ok := parsed.(map[string]interface{}); ok {
			return m
		}
		return map[string]interface{}{"raw": parsed}
	}
	return map[string]interface{}{"raw": payload}
}

// ToOpenAISlice converts Result.ToolCalls to the generic []map format.
func (r Result) ToOpenAISlice() []map[string]interface{} {
	if len(r.ToolCalls) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		out[i] = map[string]interface{}{
			"id":   tc.ID,
			"type": tc.Type,
			"function": map[string]interface{}{
				"name":      tc.Function["name"],
				"arguments": tc.Function["arguments"],
			},
		}
	}
	return out
}