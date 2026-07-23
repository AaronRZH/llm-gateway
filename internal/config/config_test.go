package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// ==================== resolveEnv ====================

func TestResolveEnv_EmptyValue(t *testing.T) {
	if got := resolveEnv(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestResolveEnv_PlainValue(t *testing.T) {
	if got := resolveEnv("plain"); got != "plain" {
		t.Errorf("expected plain, got %q", got)
	}
}

func TestResolveEnv_EnvSet(t *testing.T) {
	t.Setenv("LLM_GW_TEST_DB_PASS", "secret123")
	if got := resolveEnv("${LLM_GW_TEST_DB_PASS}"); got != "secret123" {
		t.Errorf("expected secret123, got %q", got)
	}
}

func TestResolveEnv_EnvNotSet(t *testing.T) {
	// 未设置的环境变量应原样返回引用字符串（不被替换为空）
	os.Unsetenv("LLM_GW_TEST_MISSING")
	if got := resolveEnv("${LLM_GW_TEST_MISSING}"); got != "${LLM_GW_TEST_MISSING}" {
		t.Errorf("expected unchanged ref, got %q", got)
	}
}

func TestResolveEnv_Malformed(t *testing.T) {
	// 没有闭合括号 → 不匹配前缀/后缀 → 原样返回
	if got := resolveEnv("${NOT_CLOSED"); got != "${NOT_CLOSED" {
		t.Errorf("expected unchanged, got %q", got)
	}
	// 后缀不是 } → 不匹配
	if got := resolveEnv("${VAR}x"); got != "${VAR}x" {
		t.Errorf("expected unchanged, got %q", got)
	}
}

// ==================== findMappingKey / findMappingIndex ====================

func parseYAML(t *testing.T, s string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

func TestFindMappingKey(t *testing.T) {
	doc := parseYAML(t, "providers:\n  p1:\n    base_url: http://x\nother: value\n")
	root := doc.Content[0]
	if _, n := findMappingKey(root, "providers"); n == nil {
		t.Fatal("expected to find providers key")
	}
	if _, n := findMappingKey(root, "missing"); n != nil {
		t.Error("expected nil for missing key")
	}
	if _, n := findMappingKey(&yaml.Node{Kind: yaml.ScalarNode}, "x"); n != nil {
		t.Error("expected nil for non-mapping node")
	}
}

func TestFindMappingIndex(t *testing.T) {
	doc := parseYAML(t, "a: 1\nb: 2\n")
	root := doc.Content[0]
	if idx := findMappingIndex(root, "b"); idx < 0 {
		t.Error("expected valid index for b")
	}
	if idx := findMappingIndex(root, "missing"); idx != -1 {
		t.Errorf("expected -1 for missing, got %d", idx)
	}
}

// ==================== valueToNode / setMappingKey / deleteMappingKey ====================

func TestValueToNode_Scalar(t *testing.T) {
	n := valueToNode("hello")
	if n.Kind != yaml.ScalarNode || n.Value != "hello" {
		t.Errorf("unexpected scalar node: %+v", n)
	}
}

func TestValueToNode_Struct(t *testing.T) {
	n := valueToNode(ProviderConfig{BaseURL: "http://x", Protocol: "openai"})
	if n.Kind != yaml.MappingNode {
		t.Errorf("expected mapping node, got %v", n.Kind)
	}
	if _, v := findMappingKey(n, "base_url"); v == nil {
		t.Error("expected base_url key in node")
	}
}

func TestSetMappingKey_ReplaceAndAppend(t *testing.T) {
	doc := parseYAML(t, "strategy: priority\n")
	root := doc.Content[0]
	setMappingKey(root, "strategy", "round_robin")
	if _, v := findMappingKey(root, "strategy"); v == nil || v.Value != "round_robin" {
		t.Error("expected strategy replaced to round_robin")
	}
	setMappingKey(root, "newkey", "val")
	if _, v := findMappingKey(root, "newkey"); v == nil || v.Value != "val" {
		t.Error("expected newkey appended")
	}
	// 非 mapping 节点应安全无操作
	setMappingKey(&yaml.Node{Kind: yaml.ScalarNode}, "x", "y")
}

func TestDeleteMappingKey(t *testing.T) {
	doc := parseYAML(t, "a: 1\nb: 2\n")
	root := doc.Content[0]
	deleteMappingKey(root, "a")
	if _, v := findMappingKey(root, "a"); v != nil {
		t.Error("expected a deleted")
	}
	deleteMappingKey(root, "missing") // 删除不存在的 key 应安全
	deleteMappingKey(&yaml.Node{Kind: yaml.ScalarNode}, "x")
}

// ==================== 手术式方法（基于临时文件） ====================

func writeTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
app:
  name: gw
  port: 8080
providers:
  p1:
    base_url: http://example.com
    protocol: openai
api_keys:
  - key: k1
    name: admin
models:
  - name: gpt-4
    tier: premium
real_models:
  strategy: priority
  models:
    - provider: p1
      model: m1
      weight: 1
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSaveProvider(t *testing.T) {
	path := writeTempConfig(t)
	cfg := &Config{filePath: path}
	if err := cfg.SaveProvider("p2", ProviderConfig{BaseURL: "http://b", Protocol: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg2.Providers["p2"]; !ok {
		t.Error("expected p2 saved")
	}
}

func TestDeleteProvider(t *testing.T) {
	path := writeTempConfig(t)
	cfg := &Config{filePath: path}
	if err := cfg.DeleteProvider("p1"); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg2.Providers["p1"]; ok {
		t.Error("expected p1 deleted")
	}
}

func TestAppendAPIKeyAndRemove(t *testing.T) {
	path := writeTempConfig(t)
	cfg := &Config{filePath: path}
	if err := cfg.AppendAPIKey(APIKeyConfig{Key: "k2", Name: "user2"}); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range cfg2.APIKeys {
		if k.Key == "k2" {
			found = true
		}
	}
	if !found {
		t.Error("expected k2 appended")
	}
	if err := cfg2.RemoveAPIKey("k2"); err != nil {
		t.Fatal(err)
	}
	cfg3, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range cfg3.APIKeys {
		if k.Key == "k2" {
			t.Error("expected k2 removed")
		}
	}
}

func TestAppendModelAndRemove(t *testing.T) {
	path := writeTempConfig(t)
	cfg := &Config{filePath: path}
	if err := cfg.AppendModel(ModelEntry{Name: "gpt-5", Tier: "premium"}); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range cfg2.Models {
		if m.Name == "gpt-5" {
			found = true
		}
	}
	if !found {
		t.Error("expected gpt-5 appended")
	}
	if err := cfg2.RemoveModel("gpt-5"); err != nil {
		t.Fatal(err)
	}
	cfg3, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cfg3.Models {
		if m.Name == "gpt-5" {
			t.Error("expected gpt-5 removed")
		}
	}
}

func TestSaveStrategy(t *testing.T) {
	path := writeTempConfig(t)
	cfg := &Config{filePath: path}
	if err := cfg.SaveStrategy("round_robin"); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.RealModels.Strategy != "round_robin" {
		t.Errorf("expected round_robin, got %q", cfg2.RealModels.Strategy)
	}
}

func TestAppendRealModel_Update_Delete(t *testing.T) {
	path := writeTempConfig(t)
	cfg := &Config{filePath: path}

	if err := cfg.AppendRealModel(FallbackItem{Provider: "p1", Model: "m2", Weight: 2}); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg2.RealModels.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg2.RealModels.Models))
	}

	if err := cfg2.UpdateRealModel(0, FallbackItem{Provider: "p1", Model: "mX", Weight: 9}); err != nil {
		t.Fatal(err)
	}
	cfg3, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.RealModels.Models[0].Model != "mX" {
		t.Errorf("expected mX, got %q", cfg3.RealModels.Models[0].Model)
	}

	// 越界更新应返回错误
	if err := cfg3.UpdateRealModel(99, FallbackItem{}); err == nil {
		t.Error("expected error for out-of-range update")
	}

	if err := cfg3.RemoveRealModel(0); err != nil {
		t.Fatal(err)
	}
	cfg4, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg4.RealModels.Models) != 1 {
		t.Fatalf("expected 1 model after delete, got %d", len(cfg4.RealModels.Models))
	}

	// 越界删除应返回错误
	if err := cfg4.RemoveRealModel(99); err == nil {
		t.Error("expected error for out-of-range delete")
	}
}

func TestRemoveRealModelsByProvider(t *testing.T) {
	path := writeTempConfig(t)
	cfg := &Config{filePath: path}
	if err := cfg.AppendRealModel(FallbackItem{Provider: "p2", Model: "m9"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RemoveRealModelsByProvider("p1"); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cfg2.RealModels.Models {
		if m.Provider == "p1" {
			t.Error("expected p1 models removed")
		}
	}
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTempConfig(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.App.Port)
	}
	if cfg.Health.Path != "/health" {
		t.Errorf("expected /health, got %q", cfg.Health.Path)
	}
	if cfg.App.ReadTimeout.Seconds() != 60 {
		t.Errorf("expected read_timeout 60s, got %v", cfg.App.ReadTimeout)
	}
	if cfg.Admin.JWTSecret == "" {
		t.Error("expected auto-generated JWT secret")
	}
	if _, ok := cfg.Providers["p1"]; !ok {
		t.Error("expected p1 provider parsed")
	}
}

func TestSave_NoPath(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Save(); err == nil {
		t.Error("expected error when filePath not set")
	}
}
