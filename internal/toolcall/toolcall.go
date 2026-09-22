package toolcall

import (
	"encoding/json"
	"errors"
	"fmt"
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

var ErrMalformed = errors.New("malformed tool call")

// Definition is the subset of a request tool definition needed to validate
// model-produced calls before they are forwarded to the client.
type Definition struct {
	Name       string
	Parameters any
}

// Supported open-tag families: their pattern and matching close tag.
type tagFamily struct {
	open  string
	close string
}

var (
	LT_SL = `</`
	GT    = `>`
)

var families = []tagFamily{
	{open: `<function=([a-zA-Z0-9_.-]+)>`, close: `</function>`},
	{open: `<tool_name=([a-zA-Z0-9_.-]+)>`, close: `</tool_name>`},
	{open: `<tool_call=([a-zA-Z0-9_.-]+)>`, close: `</tool_call>`},
	{open: `<invoke name=([a-zA-Z0-9_.-]+)>`, close: `</invoke>`},
}

var (
	plainWrapperRe = regexp.MustCompile(`<tool_call>([\s\S]*?)</tool_call>`)
	argPairRe      = regexp.MustCompile(`<arg_key>([\s\S]*?)</arg_key>\s*<arg_value>([\s\S]*?)</arg_value>`)
	toolNameRe     = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
	suspiciousRe   = regexp.MustCompile(`(?i)<[^>]*(tool[_:-]?(call|use|name)?|function|invoke)[^>]*>|\[(tool[_ -]?call|function[_ -]?call)\]`)
	escapedTagRe   = regexp.MustCompile(`\\?<[^>\r\n]{1,160}>`)
	knownTagRe     = regexp.MustCompile(`^</?(tool_call|arg_key|arg_value|function|tool_name|invoke|parameter)>$|^<(function|tool_name|tool_call)=[a-zA-Z0-9_.-]+>$|^<invoke name=[a-zA-Z0-9_.-]+>$|^<parameter=[a-zA-Z0-9_.-]+>$`)
)

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
//  1. Inline JSON: {"a":1}
//  2. Nested parameters: <parameter=a>1</parameter>
//
// Missing close tags are tolerated: the next sibling open tag terminates the block.
func Normalize(text string) Result {
	r := Result{}
	if text == "" {
		return r
	}
	text = canonicalizeEscapedToolTags(text)

	// Some OpenAI-compatible upstreams emit a tool call in this form:
	//   <tool_call>edit<arg_key>file_path</arg_key><arg_value>...</arg_value></tool_call>
	// Parse and remove those wrappers first. The older function=/parameter=
	// parser below then handles any remaining calls.
	text, r.ToolCalls = extractPlainWrappers(text)
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
			closeIdx = strings.Index(rest, LT_SL+name+GT)
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

// LooksLikeToolCall reports whether text contains a marker that strongly
// suggests the upstream attempted to call a tool. Callers must not silently
// downgrade such text to a normal assistant response when parsing fails.
func LooksLikeToolCall(text string) bool {
	text = canonicalizeEscapedToolTags(text)
	markers := []string{"<tool_call", "<function=", "<tool_name=", "<invoke name=", "<arg_key>"}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return suspiciousRe.MatchString(text)
}

// canonicalizeEscapedToolTags accepts the Markdown-escaped tag spelling some
// models emit, for example \<tool\_call> and \</arg\_value>. Only known markup
// tags are unescaped so backslashes inside shell commands and other argument
// values remain byte-for-byte unchanged.
func canonicalizeEscapedToolTags(text string) string {
	return escapedTagRe.ReplaceAllStringFunc(text, func(candidate string) string {
		canonical := strings.TrimPrefix(candidate, `\`)
		canonical = strings.NewReplacer(
			`\_`, `_`,
			`\/`, `/`,
			`\=`, `=`,
		).Replace(canonical)
		if knownTagRe.MatchString(canonical) {
			return canonical
		}
		return candidate
	})
}

// Validate checks tool names, JSON argument shape, required properties, and
// the common primitive JSON-schema types. It deliberately stays conservative:
// unknown schema keywords remain the upstream/client's responsibility.
func Validate(calls []OpenAIToolCall, definitions []Definition) error {
	if len(calls) == 0 {
		return fmt.Errorf("%w: no calls parsed", ErrMalformed)
	}
	allowed := make(map[string]Definition, len(definitions))
	for _, def := range definitions {
		if def.Name != "" {
			allowed[def.Name] = def
		}
	}
	for _, call := range calls {
		name, _ := call.Function["name"].(string)
		def, ok := allowed[name]
		if !ok {
			return fmt.Errorf("%w: tool %q was not offered", ErrMalformed, name)
		}
		raw, _ := call.Function["arguments"].(string)
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return fmt.Errorf("%w: arguments for %q are not a JSON object: %v", ErrMalformed, name, err)
		}
		if err := validateArguments(args, def.Parameters); err != nil {
			return fmt.Errorf("%w: arguments for %q: %v", ErrMalformed, name, err)
		}
	}
	return nil
}

// ValidateMaps validates the generic OpenAI tool_calls representation used by
// the gateway's response and streaming layers.
func ValidateMaps(calls []map[string]interface{}, definitions []Definition) error {
	typed := make([]OpenAIToolCall, 0, len(calls))
	for _, call := range calls {
		fn, ok := call["function"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("%w: missing function object", ErrMalformed)
		}
		typed = append(typed, OpenAIToolCall{
			ID:       stringValue(call["id"]),
			Type:     stringValue(call["type"]),
			Function: fn,
		})
	}
	return Validate(typed, definitions)
}

func stringValue(v interface{}) string {
	s, _ := v.(string)
	return s
}

func validateArguments(args map[string]interface{}, schema any) error {
	s, ok := schema.(map[string]interface{})
	if !ok || s == nil {
		return nil
	}
	if required, ok := s["required"].([]interface{}); ok {
		for _, item := range required {
			name, _ := item.(string)
			if name != "" {
				if _, exists := args[name]; !exists {
					return fmt.Errorf("missing required property %q", name)
				}
			}
		}
	}
	if required, ok := s["required"].([]string); ok {
		for _, name := range required {
			if _, exists := args[name]; !exists {
				return fmt.Errorf("missing required property %q", name)
			}
		}
	}
	properties, _ := s["properties"].(map[string]interface{})
	for name, value := range args {
		property, _ := properties[name].(map[string]interface{})
		expected, _ := property["type"].(string)
		if expected == "" {
			continue
		}
		valid := false
		switch expected {
		case "string":
			_, valid = value.(string)
		case "number":
			_, valid = value.(float64)
		case "integer":
			n, numeric := value.(float64)
			valid = numeric && n == float64(int64(n))
		case "boolean":
			_, valid = value.(bool)
		case "object":
			_, valid = value.(map[string]interface{})
		case "array":
			_, valid = value.([]interface{})
		case "null":
			valid = value == nil
		default:
			valid = true
		}
		if !valid {
			return fmt.Errorf("property %q must be %s", name, expected)
		}
	}
	return nil
}

func extractPlainWrappers(text string) (string, []OpenAIToolCall) {
	var calls []OpenAIToolCall
	clean := plainWrapperRe.ReplaceAllStringFunc(text, func(block string) string {
		match := plainWrapperRe.FindStringSubmatch(block)
		if len(match) != 2 {
			return block
		}
		body := match[1]
		firstArg := strings.Index(body, "<arg_key>")
		if firstArg < 0 {
			return block
		}
		name := strings.TrimSpace(body[:firstArg])
		if !toolNameRe.MatchString(name) {
			return block
		}
		pairs := argPairRe.FindAllStringSubmatch(body[firstArg:], -1)
		if len(pairs) == 0 {
			return block
		}
		if leftover := strings.TrimSpace(argPairRe.ReplaceAllString(body[firstArg:], "")); leftover != "" {
			return block
		}
		args := make(map[string]interface{}, len(pairs))
		for _, pair := range pairs {
			key := strings.TrimSpace(pair[1])
			if key == "" {
				return block
			}
			if _, duplicate := args[key]; duplicate {
				return block
			}
			args[key] = pair[2]
		}
		argsJSON, _ := json.Marshal(args)
		calls = append(calls, OpenAIToolCall{
			ID:   "call_" + uuid.New().String()[:8],
			Type: "function",
			Function: map[string]interface{}{
				"name":      name,
				"arguments": string(argsJSON),
			},
		})
		return ""
	})
	return clean, calls
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
