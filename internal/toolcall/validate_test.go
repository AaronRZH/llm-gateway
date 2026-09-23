package toolcall

import (
	"strings"
	"testing"
)

// 本文件为 validateArguments 补充 characterization tests，锁定行为作为重构安全网。
// validateArguments 当前覆盖率 24.2%，重构后目标 ≥ 95%。

func TestValidateArguments_NilSchema(t *testing.T) {
	args := map[string]interface{}{"foo": "bar"}
	if err := validateArguments(args, nil); err != nil {
		t.Errorf("nil schema should return nil, got: %v", err)
	}
}

func TestValidateArguments_NonMapSchema(t *testing.T) {
	args := map[string]interface{}{"foo": "bar"}
	if err := validateArguments(args, "not a map"); err != nil {
		t.Errorf("non-map schema should return nil, got: %v", err)
	}
}

func TestValidateArguments_EmptySchema(t *testing.T) {
	args := map[string]interface{}{"foo": "bar"}
	schema := map[string]interface{}{}
	if err := validateArguments(args, schema); err != nil {
		t.Errorf("empty schema should return nil, got: %v", err)
	}
}

// required 作为 []interface{} 形式
func TestValidateArguments_RequiredAsInterfaceSlice(t *testing.T) {
	schema := map[string]interface{}{
		"required": []interface{}{"foo", "bar"},
	}
	args := map[string]interface{}{"foo": "a", "bar": "b"}
	if err := validateArguments(args, schema); err != nil {
		t.Errorf("all required present should pass, got: %v", err)
	}
	args = map[string]interface{}{"foo": "a"}
	err := validateArguments(args, schema)
	if err == nil {
		t.Error("expected error for missing required property")
	} else if !strings.Contains(err.Error(), "missing required property \"bar\"") {
		t.Errorf("expected missing bar error, got: %v", err)
	}
}

// required 作为 []string 形式（JSON 解析后可能为 []interface{} 或 []string）
func TestValidateArguments_RequiredAsStringSlice(t *testing.T) {
	schema := map[string]interface{}{
		"required": []string{"foo", "bar"},
	}
	args := map[string]interface{}{"foo": "a", "bar": "b"}
	if err := validateArguments(args, schema); err != nil {
		t.Errorf("all required present should pass, got: %v", err)
	}
	args = map[string]interface{}{"foo": "a"}
	err := validateArguments(args, schema)
	if err == nil {
		t.Error("expected error for missing required property")
	}
}

// required 包含空字符串项应被跳过
func TestValidateArguments_RequiredEmptyStringSkipped(t *testing.T) {
	schema := map[string]interface{}{
		"required": []interface{}{"", "foo"},
	}
	args := map[string]interface{}{"foo": "a"}
	if err := validateArguments(args, schema); err != nil {
		t.Errorf("empty string in required should be skipped, got: %v", err)
	}
}

// 类型验证测试
func TestValidateArguments_TypeChecks(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]interface{}
		schema      map[string]interface{}
		expectErr   bool
		errFragment string
	}{
		{
			name:      "string_ok",
			args:      map[string]interface{}{"name": "hello"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}},
			expectErr: false,
		},
		{
			name:        "string_wrong_type",
			args:        map[string]interface{}{"name": 123},
			schema:      map[string]interface{}{"properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}},
			expectErr:   true,
			errFragment: "property \"name\" must be string",
		},
		{
			name:      "number_ok_float64",
			args:      map[string]interface{}{"count": float64(42)},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"count": map[string]interface{}{"type": "number"}}},
			expectErr: false,
		},
		{
			name:      "number_wrong_type",
			args:      map[string]interface{}{"count": "42"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"count": map[string]interface{}{"type": "number"}}},
			expectErr: true,
		},
		{
			name:      "integer_ok_float64_whole",
			args:      map[string]interface{}{"count": float64(42)},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"count": map[string]interface{}{"type": "integer"}}},
			expectErr: false,
		},
		{
			name:      "integer_fail_fractional",
			args:      map[string]interface{}{"count": float64(42.5)},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"count": map[string]interface{}{"type": "integer"}}},
			expectErr: true,
		},
		{
			name:      "integer_wrong_type",
			args:      map[string]interface{}{"count": "42"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"count": map[string]interface{}{"type": "integer"}}},
			expectErr: true,
		},
		{
			name:      "boolean_ok",
			args:      map[string]interface{}{"flag": true},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"flag": map[string]interface{}{"type": "boolean"}}},
			expectErr: false,
		},
		{
			name:      "boolean_wrong_type",
			args:      map[string]interface{}{"flag": "yes"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"flag": map[string]interface{}{"type": "boolean"}}},
			expectErr: true,
		},
		{
			name:      "object_ok",
			args:      map[string]interface{}{"meta": map[string]interface{}{"a": 1}},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"meta": map[string]interface{}{"type": "object"}}},
			expectErr: false,
		},
		{
			name:      "object_wrong_type",
			args:      map[string]interface{}{"meta": "not object"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"meta": map[string]interface{}{"type": "object"}}},
			expectErr: true,
		},
		{
			name:      "array_ok",
			args:      map[string]interface{}{"items": []interface{}{1, 2, 3}},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"items": map[string]interface{}{"type": "array"}}},
			expectErr: false,
		},
		{
			name:      "array_wrong_type",
			args:      map[string]interface{}{"items": "not array"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"items": map[string]interface{}{"type": "array"}}},
			expectErr: true,
		},
		{
			name:      "null_ok",
			args:      map[string]interface{}{"v": nil},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"v": map[string]interface{}{"type": "null"}}},
			expectErr: false,
		},
		{
			name:      "null_wrong_type",
			args:      map[string]interface{}{"v": "not null"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"v": map[string]interface{}{"type": "null"}}},
			expectErr: true,
		},
		{
			// 未知类型（非标准 JSON Schema 类型）默认为 valid
			name:      "unknown_type_passes",
			args:      map[string]interface{}{"v": "anything"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"v": map[string]interface{}{"type": "custom_type"}}},
			expectErr: false,
		},
		{
			// properties 中缺少某属性定义时跳过类型校验
			name:      "missing_property_def_skips",
			args:      map[string]interface{}{"foo": "bar"},
			schema:    map[string]interface{}{"properties": map[string]interface{}{}},
			expectErr: false,
		},
		{
			// property 无 type 字段时跳过类型校验
			name:      "property_no_type_skips",
			args:      map[string]interface{}{"foo": 123},
			schema:    map[string]interface{}{"properties": map[string]interface{}{"foo": map[string]interface{}{}}},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArguments(tc.args, tc.schema)
			if tc.expectErr && err == nil {
				t.Errorf("expected error, got nil")
				return
			}
			if !tc.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if tc.expectErr && err != nil && tc.errFragment != "" {
				if !strings.Contains(err.Error(), tc.errFragment) {
					t.Errorf("expected error containing %q, got: %v", tc.errFragment, err)
				}
			}
		})
	}
}

// TestValidateArguments_MultipleErrors 验证返回第一个错误即停止
func TestValidateArguments_MultipleErrors(t *testing.T) {
	schema := map[string]interface{}{
		"required": []interface{}{"missing", "type_wrong"},
		"properties": map[string]interface{}{
			"type_wrong": map[string]interface{}{"type": "string"},
		},
	}
	args := map[string]interface{}{"type_wrong": 123}
	err := validateArguments(args, schema)
	if err == nil {
		t.Error("expected error")
	}
	// required 检查先于类型检查，应先报 missing required
	if !strings.Contains(err.Error(), "missing required property \"missing\"") {
		t.Errorf("expected required error first, got: %v", err)
	}
}

// TestValidateArguments_NoProperties 验证无 properties 时仅检查 required
func TestValidateArguments_NoProperties(t *testing.T) {
	schema := map[string]interface{}{
		"required": []interface{}{"foo"},
	}
	args := map[string]interface{}{"foo": "bar", "extra": "anything"}
	if err := validateArguments(args, schema); err != nil {
		t.Errorf("extra fields should pass, got: %v", err)
	}
}

// TestValidateArguments_SchemaNotMapInProperties 验证 properties 中非 map 属性被跳过
func TestValidateArguments_SchemaNotMapInProperties(t *testing.T) {
	schema := map[string]interface{}{
		"properties": map[string]interface{}{
			"foo": "not a map",
		},
	}
	args := map[string]interface{}{"foo": "bar"}
	if err := validateArguments(args, schema); err != nil {
		t.Errorf("non-map property def should be skipped, got: %v", err)
	}
}

// TestValidateArguments_ErrContainsPropertyPath 验证错误信息包含属性名
func TestValidateArguments_ErrContainsPropertyPath(t *testing.T) {
	schema := map[string]interface{}{
		"required": []interface{}{"name"},
	}
	args := map[string]interface{}{}
	err := validateArguments(args, schema)
	if err == nil {
		t.Error("expected non-nil error")
		return
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention property name, got: %v", err)
	}
}
