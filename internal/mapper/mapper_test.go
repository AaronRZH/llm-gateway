package mapper

import (
	"testing"

	"llm-gateway/internal/config"
)

func TestValidate(t *testing.T) {
	s := New([]config.ModelEntry{{Name: "gpt-4", Tier: "premium"}})
	if err := s.Validate("gpt-4"); err != nil {
		t.Errorf("expected valid, got %v", err)
	}
	if err := s.Validate("unknown"); err == nil {
		t.Error("expected error for unknown model")
	}
	// 空名
	if err := s.Validate(""); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestGetTier(t *testing.T) {
	s := New([]config.ModelEntry{{Name: "gpt-4", Tier: "premium"}, {Name: "gpt-3"}})
	if tier := s.GetTier("gpt-4"); tier != "premium" {
		t.Errorf("expected premium, got %q", tier)
	}
	// 无 tier 返回空
	if tier := s.GetTier("gpt-3"); tier != "" {
		t.Errorf("expected empty tier, got %q", tier)
	}
	// 不存在返回空
	if tier := s.GetTier("missing"); tier != "" {
		t.Errorf("expected empty for missing, got %q", tier)
	}
}

func TestListVirtualModels(t *testing.T) {
	s := New([]config.ModelEntry{{Name: "a", Tier: "t1"}, {Name: "b"}})
	models := s.ListVirtualModels()
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	// tier 应只存在于有 tier 的项
	for _, m := range models {
		if m["id"] == "a" {
			if m["tier"] != "t1" {
				t.Errorf("expected tier t1, got %v", m["tier"])
			}
		}
		if m["id"] == "b" {
			if _, ok := m["tier"]; ok {
				t.Error("expected no tier for b")
			}
		}
	}
	// 空列表
	empty := New(nil)
	if models := empty.ListVirtualModels(); len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}

func TestAddModel(t *testing.T) {
	s := New(nil)
	if !s.AddModel("gpt-4", "premium") {
		t.Error("expected add to succeed")
	}
	// 重复添加应失败
	if s.AddModel("gpt-4", "premium") {
		t.Error("expected duplicate add to fail")
	}
	// 空名应失败
	if s.AddModel("", "x") {
		t.Error("expected empty name to fail")
	}
	if err := s.Validate("gpt-4"); err != nil {
		t.Errorf("expected added model valid, got %v", err)
	}
}

func TestDeleteModel(t *testing.T) {
	s := New([]config.ModelEntry{{Name: "gpt-4"}})
	if !s.DeleteModel("gpt-4") {
		t.Error("expected delete to succeed")
	}
	// 删除不存在应失败
	if s.DeleteModel("gpt-4") {
		t.Error("expected delete of missing to fail")
	}
	// 空名应失败
	if s.DeleteModel("") {
		t.Error("expected empty name delete to fail")
	}
}

func TestRewriteResponse(t *testing.T) {
	s := New(nil)
	// 有效 JSON 含 model 字段 → 重写
	body := []byte(`{"model":"gpt-4o","choices":[]}`)
	out := s.RewriteResponse(body, "virtual-model")
	if string(out) == string(body) {
		t.Fatal("expected body to be rewritten")
	}
	if !contains(string(out), "virtual-model") {
		t.Errorf("expected virtual-model in output, got %s", string(out))
	}

	// 无 model 字段 → 原样返回（但会重新 marshal，内容等价）
	body2 := []byte(`{"id":"1","object":"list"}`)
	out2 := s.RewriteResponse(body2, "vm")
	if !contains(string(out2), `"id":"1"`) {
		t.Errorf("expected id preserved, got %s", string(out2))
	}

	// 非法 JSON → 原样返回（同一份字节）
	bad := []byte(`{not valid json`)
	out3 := s.RewriteResponse(bad, "vm")
	if string(out3) != string(bad) {
		t.Errorf("expected unchanged bytes for invalid JSON, got %s", string(out3))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
