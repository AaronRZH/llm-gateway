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

// openTagRe returns the compiled open-tag regexp. The tag spelling is
// assembled at runtime to avoid raw XML literals in this file.
func openTagRe() *regexp.Regexp {
	return regexp.MustCompile(`<function=([a-zA-Z0-9_-]+)>`)
}

// Normalize scans text for embedded XML tool-call tags and converts each one
// into an OpenAI-shaped tool call, stripping the matching XML from the text.
//
// Supported shape (function name must match [a-zA-Z0-9_-]+):
//
//	<function=foo>{"a":1}</function>
//	<function=foo>{"a":1}
//	<function=foo>
//
// The payload (text between the opening and closing tag, or until end-of-string)
// is treated as the arguments JSON. If it does not parse as JSON, the raw string
// is kept under a "raw" key so the call is not lost.
func Normalize(text string) Result {
	r := Result{}
	if text == "" {
		return r
	}

	re := openTagRe()
	indices := re.FindAllStringSubmatchIndex(text, -1)
	if len(indices) == 0 {
		r.CleanContent = text
		return r
	}

	var cleanParts []string
	pos := 0
	for _, m := range indices {
		// m[0],m[1] = full open tag; m[2],m[3] = captured name
		cleanParts = append(cleanParts, text[pos:m[0]])
		name := text[m[2]:m[3]]

		// Payload = everything after '>' of the open tag until either:
		//   - the matching closing tag </name> (if present),
		//   - the next opening tag <function= (a following sibling call), or
		//   - end-of-string.
		afterOpen := m[1]
		if afterOpen >= len(text) {
			continue
		}
		closeTag := "</" + name + ">"
		closeIdx := strings.Index(text[afterOpen:], closeTag)
		nextSib := strings.Index(text[afterOpen:], "<function=")
		end := -1
		chosen := -1
		if closeIdx >= 0 && (end < 0 || closeIdx < end) {
			end = closeIdx
			chosen = 0
		}
		if nextSib >= 0 && (end < 0 || nextSib < end) {
			end = nextSib
			chosen = 1
		}

		var payload string
		if end >= 0 && chosen == 0 {
			payload = text[afterOpen : afterOpen+end]
			pos = afterOpen + end + len(closeTag)
		} else if end >= 0 && chosen == 1 {
			payload = text[afterOpen : afterOpen+end]
			// pos stays; the sibling's own open tag is picked up next iteration
		} else {
			payload = text[afterOpen:]
			pos = len(text)
		}

		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}

		var args interface{}
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			args = map[string]interface{}{"raw": payload}
		}
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
	r.CleanContent = strings.Join(cleanParts, "")
	r.CleanContent = strings.TrimSpace(r.CleanContent)
	return r
}

// ToOpenAISlice converts Result.ToolCalls to the generic []map format used by
// the gateway's streaming accumulator and non-streaming body rewriter.
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