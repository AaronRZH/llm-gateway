package provider

import (
	"testing"
)

// 本文件为 ContentToBlocks 补充 characterization tests，锁定未覆盖分支的行为，
// 使其成为后续重构的安全网。已覆盖的 string/array/nil/float/bool/map/tool_use/text
// 分支由 internal/protocol/protocol_test.go 的跨包测试覆盖，此处不重复。

// blockType 定义一种 content block 类型及其缺失时的必需字段默认值，
// 用于表格驱动地校验字段补全逻辑。
type blockType struct {
	name     string // content block type
	required []struct {
		field string
		isNil bool // 期望补全后字段值为 nil（Go 中 nil 与缺失 key 语义不同）
	}
}

func TestContentToBlocks_FieldCompletion(t *testing.T) {
	// 每种 content block type 的必需字段及其补全默认值。
	// source 类字段补全为 nil（Anthropic API 允许），其余补全为空值。
	cases := []struct {
		blockType  string
		expectKeys []string // 补全后必须存在的 key
	}{
		{"text", []string{"type", "text"}},
		{"image", []string{"type", "source"}},
		{"tool_use", []string{"type", "id", "name", "input"}},
		{"tool_result", []string{"type", "tool_use_id", "content"}},
		{"thinking", []string{"type", "thinking"}},
		{"document", []string{"type", "source"}},
		{"input_audio", []string{"type", "input_audio"}},
	}

	for _, tc := range cases {
		t.Run(tc.blockType, func(t *testing.T) {
			converter := NewAnthropicConverter()
			input := []interface{}{map[string]interface{}{"type": tc.blockType}}
			result := converter.ContentToBlocks(input)
			if len(result) != 1 {
				t.Fatalf("%s: expected 1 block, got %d", tc.blockType, len(result))
			}
			for _, key := range tc.expectKeys {
				if _, ok := result[0][key]; !ok {
					t.Errorf("%s: expected key %q to be filled in, got: %v", tc.blockType, key, result[0])
				}
			}
		})
	}
}

// TestContentToBlocks_ExistingFieldsPreserved 验证已存在的字段不会被覆盖。
func TestContentToBlocks_ExistingFieldsPreserved(t *testing.T) {
	converter := NewAnthropicConverter()
	input := []interface{}{
		map[string]interface{}{
			"type":  "tool_use",
			"id":    "call_keepme",
			"name":  "search",
			"input": map[string]interface{}{"q": "hello"},
		},
	}
	result := converter.ContentToBlocks(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}
	if result[0]["id"] != "call_keepme" {
		t.Errorf("id should be preserved, got: %v", result[0]["id"])
	}
	if result[0]["name"] != "search" {
		t.Errorf("name should be preserved, got: %v", result[0]["name"])
	}
}

// TestContentToBlocks_ArrayWithStringNumberBool 验证数组中混合标量元素的转换。
func TestContentToBlocks_ArrayWithStringNumberBool(t *testing.T) {
	converter := NewAnthropicConverter()
	input := []interface{}{"hello", 42.0, true}
	result := converter.ContentToBlocks(input)
	if len(result) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(result))
	}
	for i, b := range result {
		if b["type"] != "text" {
			t.Errorf("block %d: expected type text, got %v", i, b["type"])
		}
		if _, ok := b["text"].(string); !ok {
			t.Errorf("block %d: expected text to be string, got %T", i, b["text"])
		}
	}
}

// TestContentToBlocks_UnknownBlockTypePreserved 验证未知 block type 原样保留、不做字段补全。
func TestContentToBlocks_UnknownBlockTypePreserved(t *testing.T) {
	converter := NewAnthropicConverter()
	input := []interface{}{map[string]interface{}{"type": "custom_type", "foo": "bar"}}
	result := converter.ContentToBlocks(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}
	if result[0]["type"] != "custom_type" || result[0]["foo"] != "bar" {
		t.Errorf("unknown block should be preserved as-is, got: %v", result[0])
	}
}

// TestContentToBlocks_NestedArrayIgnored 验证数组中嵌套数组被忽略。
func TestContentToBlocks_NestedArrayIgnored(t *testing.T) {
	converter := NewAnthropicConverter()
	input := []interface{}{"keep", []interface{}{"nested", "ignored"}}
	result := converter.ContentToBlocks(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 block (nested array ignored), got %d", len(result))
	}
}

// TestContentToBlocks_OnlySkippedElementsReturnsEmptyBlock 验证数组中仅含被跳过的元素时返回空文本块。
func TestContentToBlocks_OnlySkippedElementsReturnsEmptyBlock(t *testing.T) {
	converter := NewAnthropicConverter()
	input := []interface{}{nil, nil}
	result := converter.ContentToBlocks(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 empty text block, got %d", len(result))
	}
	if result[0]["type"] != "text" || result[0]["text"] != "" {
		t.Errorf("expected empty text block, got: %v", result[0])
	}
}

// TestContentToBlocks_MapWithNoType 验证无 type 字段的 map 不做任何补全。
func TestContentToBlocks_MapWithNoType(t *testing.T) {
	converter := NewAnthropicConverter()
	input := []interface{}{map[string]interface{}{"foo": "bar"}}
	result := converter.ContentToBlocks(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}
	if result[0]["foo"] != "bar" {
		t.Errorf("map should be preserved, got: %v", result[0])
	}
	// 不应添加 text 字段（因为没有 type 触发补全逻辑）
	if _, ok := result[0]["text"]; ok {
		t.Errorf("map without type should not have text added, got: %v", result[0])
	}
}

// TestContentToBlocks_UnknownTopLevelType 验证未知顶层类型（如数组）转换为空文本块。
func TestContentToBlocks_UnknownTopLevelType(t *testing.T) {
	converter := NewAnthropicConverter()
	result := converter.ContentToBlocks([]interface{}{"a", "b"})
	if len(result) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(result))
	}
}

// TestContentToBlocks_NilContent 验证顶层 nil 返回空文本块。
func TestContentToBlocks_NilContent(t *testing.T) {
	converter := NewAnthropicConverter()
	result := converter.ContentToBlocks(nil)
	if len(result) != 1 {
		t.Fatalf("expected 1 empty text block, got %d", len(result))
	}
	if result[0]["type"] != "text" || result[0]["text"] != "" {
		t.Errorf("expected empty text block for nil, got: %v", result[0])
	}
}

// TestContentToBlocks_SingleMapNoType 验证无 type 字段的单个 map 原样返回。
func TestContentToBlocks_SingleMapNoType(t *testing.T) {
	converter := NewAnthropicConverter()
	input := map[string]interface{}{"foo": "bar"}
	result := converter.ContentToBlocks(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}
	if result[0]["foo"] != "bar" {
		t.Errorf("map should be preserved, got: %v", result[0])
	}
	// 不应添加 type 字段（因为没有 type 触发补全逻辑）
	if _, ok := result[0]["type"]; ok {
		t.Errorf("map without type should not have type added, got: %v", result[0])
	}
}
