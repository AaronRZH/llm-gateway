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
