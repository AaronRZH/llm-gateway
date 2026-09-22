package toolcall

import (
	"encoding/json"
	"testing"
)

func TestNormalize_ToolCallWrapper(t *testing.T) {
	L := string(rune(0x3c))
	G := string(rune(0x3e))
	S := string(rune(0x2f))
	NL := string(rune(0x0a))

	// 外层 <tool_call> 包裹 <function=...> + <parameter> 的格式。
	text := "Think" + NL +
		L + "tool_call" + G + NL +
		L + "function=run_code" + G + NL +
		L + "parameter=code" + G + NL +
		"await tools.edit({ file_path: /tmp/x.ts, new_string: x });" + NL +
		L + S + "parameter" + G + NL +
		L + "parameter=description" + G + NL +
		"Add TABLE_RELATIONSHIPS import" + NL +
		L + S + "parameter" + G + NL +
		L + S + "function" + G + NL +
		L + S + "tool_call" + G

	t.Logf("input=%q", text)

	r := Normalize(text)
	t.Logf("ToolCalls=%d CleanContent=%q", len(r.ToolCalls), r.CleanContent)

	if len(r.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (CleanContent=%q)", len(r.ToolCalls), r.CleanContent)
	}
	if r.ToolCalls[0].Function["name"] != "run_code" {
		t.Fatalf("unexpected name %v", r.ToolCalls[0].Function["name"])
	}
	args := r.ToolCalls[0].Function["arguments"].(string)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("args not valid JSON: %v (raw=%q)", err, args)
	}
	for _, k := range []string{"code", "description"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in args: %v", k, m)
		}
	}
}

func TestNormalize_ArgKeyValueWrapper(t *testing.T) {
	text := `<tool_call>edit` +
		`<arg_key>file_path</arg_key><arg_value>/tmp/server.ts</arg_value>` +
		`<arg_key>new_string</arg_key><arg_value>new value</arg_value>` +
		`<arg_key>old_string</arg_key><arg_value>old value</arg_value>` +
		`</tool_call>`

	r := Normalize(text)
	if r.CleanContent != "" {
		t.Fatalf("expected no leftover content, got %q", r.CleanContent)
	}
	if len(r.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(r.ToolCalls))
	}
	if r.ToolCalls[0].Function["name"] != "edit" {
		t.Fatalf("unexpected name %v", r.ToolCalls[0].Function["name"])
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(r.ToolCalls[0].Function["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args["file_path"] != "/tmp/server.ts" || args["new_string"] != "new value" || args["old_string"] != "old value" {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestValidateRejectsUnknownToolAndMissingRequiredArgument(t *testing.T) {
	definitions := []Definition{{
		Name: "edit",
		Parameters: map[string]interface{}{
			"type":     "object",
			"required": []interface{}{"file_path"},
			"properties": map[string]interface{}{
				"file_path": map[string]interface{}{"type": "string"},
			},
		},
	}}

	unknown := Normalize(xmlOpenForWrapperTest("delete") + `{"file_path":"/tmp/a"}`)
	if err := Validate(unknown.ToolCalls, definitions); err == nil {
		t.Fatal("expected unknown tool to be rejected")
	}
	missing := Normalize(xmlOpenForWrapperTest("edit") + `{}`)
	if err := Validate(missing.ToolCalls, definitions); err == nil {
		t.Fatal("expected missing required argument to be rejected")
	}
}

func xmlOpenForWrapperTest(name string) string {
	return "<" + "function=" + name + ">"
}
