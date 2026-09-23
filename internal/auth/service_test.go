package auth

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newMiniRedisClient 启动一个进程内 miniredis 并返回其客户端，避免测试依赖真实 Redis。
func newMiniRedisClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// newBrokenRedisClient 返回一个指向已关闭端口的客户端，用于覆盖 Redis 失败回退分支。
func newBrokenRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	addr := mr.Addr()
	mr.Close() // 关闭后端口拒绝连接
	rdb := redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: 200 * time.Millisecond,
		ReadTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func seeds(m map[string]string) map[string]*KeyInfo {
	out := make(map[string]*KeyInfo, len(m))
	for k, name := range m {
		out[k] = &KeyInfo{Key: k, Name: name, CreatedAt: time.Now()}
	}
	return out
}

func TestNew_NilSeedsDoesNotPanic(t *testing.T) {
	svc := New(nil, nil)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	if len(svc.ListSeedKeys()) != 0 {
		t.Fatal("expected no seed keys")
	}
	if _, ok := svc.Validate("anything"); ok {
		t.Fatal("empty service must reject unknown key")
	}
}

func TestValidate_EmptyKey(t *testing.T) {
	svc := New(nil, nil)
	if _, ok := svc.Validate(""); ok {
		t.Fatal("empty key must be rejected")
	}
}

func TestValidate_SeedKeyHitAndLocalCache(t *testing.T) {
	svc := New(nil, seeds(map[string]string{"seed-1": "client-1"}))

	info, ok := svc.Validate("seed-1")
	if !ok || info == nil || info.Name != "client-1" {
		t.Fatalf("expected seed key hit, got ok=%v info=%+v", ok, info)
	}

	// 第二次命中本地缓存分支。
	info2, ok := svc.Validate("seed-1")
	if !ok || info2.Name != "client-1" {
		t.Fatalf("expected cache hit, got ok=%v info=%+v", ok, info2)
	}

	// 未知 Key 应被拒绝。
	if _, ok := svc.Validate("nope"); ok {
		t.Fatal("unknown key must be rejected")
	}
}

func TestCheckCache_ExpiredEntry(t *testing.T) {
	svc := New(nil, nil)
	svc.cache["k"] = &cacheEntry{info: &KeyInfo{Key: "k"}, expiresAt: time.Now().Add(-time.Second)}
	if info := svc.checkCache("k"); info != nil {
		t.Fatalf("expired cache entry must be ignored, got %+v", info)
	}
}

func TestSetCache_CleansExpiredWhenLarge(t *testing.T) {
	svc := New(nil, nil)
	for i := 0; i < 10001; i++ {
		svc.cache[fmt.Sprintf("k%d", i)] = &cacheEntry{expiresAt: time.Now().Add(-time.Hour)}
	}
	svc.setCache("fresh", &KeyInfo{Key: "fresh", Name: "n"})

	if _, ok := svc.cache["fresh"]; !ok {
		t.Fatal("fresh entry must exist after cleanup")
	}
	if len(svc.cache) > 2 {
		t.Fatalf("expected expired entries pruned, got %d entries", len(svc.cache))
	}
}

func TestValidate_RedisHitAndMiss(t *testing.T) {
	mr, rdb := newMiniRedisClient(t)

	// 直接向 miniredis 注入一个 Key（非种子 Key），验证 Redis 命中分支。
	mr.HSet(apikeyPrefix+"redis-key", "name", "from-redis")

	// 使用 nil seeds，避免异步同步 goroutine 与断言并发。
	svc := New(rdb, nil)

	info, ok := svc.Validate("redis-key")
	if !ok || info == nil || info.Name != "from-redis" {
		t.Fatalf("expected redis hit, got ok=%v info=%+v", ok, info)
	}

	// Redis 中不存在的 Key 应被拒绝。
	if _, ok := svc.Validate("missing-key"); ok {
		t.Fatal("missing key must be rejected")
	}
}

func TestValidate_RedisErrorFallsBackGracefully(t *testing.T) {
	rdb := newBrokenRedisClient(t)
	svc := New(rdb, nil)
	if _, ok := svc.Validate("whatever"); ok {
		t.Fatal("redis error must not authenticate the key")
	}
}

func TestCheckRedis_Error(t *testing.T) {
	rdb := newBrokenRedisClient(t)
	svc := New(rdb, nil)
	if info, ok := svc.checkRedis("x"); ok || info != nil {
		t.Fatalf("expected error fallback, got ok=%v info=%+v", ok, info)
	}
}

func TestFindKeyByName(t *testing.T) {
	svc := New(nil, seeds(map[string]string{"a": "alpha", "b": "beta"}))

	if info, ok := svc.FindKeyByName("beta"); !ok || info.Key != "b" {
		t.Fatalf("expected to find 'b', got ok=%v info=%+v", ok, info)
	}
	if _, ok := svc.FindKeyByName("ghost"); ok {
		t.Fatal("expected miss for unknown name")
	}
}

func TestListSeedKeys_ReturnsCopies(t *testing.T) {
	svc := New(nil, seeds(map[string]string{"a": "alpha"}))
	got := svc.ListSeedKeys()
	if len(got) != 1 {
		t.Fatalf("expected 1 key, got %d", len(got))
	}
	got[0].Name = "mutated"
	// 再次读取不应受外部修改影响。
	again := svc.ListSeedKeys()
	if again[0].Name != "alpha" {
		t.Fatalf("ListSeedKeys must return copies, got %q", again[0].Name)
	}
}

func TestCreateSeedKey(t *testing.T) {
	svc := New(nil, nil)

	if ok := svc.CreateSeedKey("", "n"); ok {
		t.Fatal("empty key must not be created")
	}
	if ok := svc.CreateSeedKey("k1", "first"); !ok {
		t.Fatal("expected create success")
	}
	if ok := svc.CreateSeedKey("k1", "dup"); ok {
		t.Fatal("duplicate key must not be created")
	}
	if _, ok := svc.Validate("k1"); !ok {
		t.Fatal("created key must be valid")
	}
}

func TestCreateSeedKey_SyncsToRedis(t *testing.T) {
	mr, rdb := newMiniRedisClient(t)
	svc := New(rdb, nil)

	if ok := svc.CreateSeedKey("k1", "first"); !ok {
		t.Fatal("expected create success")
	}
	// 缺陷回归：原实现在 HSet 后追加 pipe.Expire(key, 0)，而 Redis 语义下 0 TTL 等价于立即 DEL，
	// 导致刚写入的种子 Key 被删除、checkRedis 的 EXISTS 分支永远为 0（见 DEBT.md 2.3）。
	// 修复为不再设置 TTL，故此处断言 Key 存在且无 TTL，Redis 回退分支才可被真实命中。
	if !mr.Exists(apikeyPrefix + "k1") {
		t.Fatal("seeded key must persist in redis (no TTL is applied)")
	}
	if ttl := mr.TTL(apikeyPrefix + "k1"); ttl != 0 {
		t.Fatalf("expected no expiry, got TTL %v", ttl)
	}
}

func TestCreateSeedKey_RedisError(t *testing.T) {
	rdb := newBrokenRedisClient(t)
	svc := New(rdb, nil)
	// 应当仍返回成功（同步失败仅告警），不能因 Redis 不可用而拒绝创建。
	if ok := svc.CreateSeedKey("k1", "first"); !ok {
		t.Fatal("create must succeed even if redis sync fails")
	}
}

func TestDeleteSeedKey(t *testing.T) {
	svc := New(nil, seeds(map[string]string{"k1": "first"}))
	svc.setCache("k1", &KeyInfo{Key: "k1", Name: "first"})

	if ok := svc.DeleteSeedKey(""); ok {
		t.Fatal("empty key must not be deleted")
	}
	if ok := svc.DeleteSeedKey("ghost"); ok {
		t.Fatal("missing key must not report success")
	}
	if ok := svc.DeleteSeedKey("k1"); !ok {
		t.Fatal("expected delete success")
	}
	if _, ok := svc.Validate("k1"); ok {
		t.Fatal("deleted key must no longer be valid")
	}
	if _, ok := svc.cache["k1"]; ok {
		t.Fatal("deleted key must be evicted from local cache")
	}
}

func TestDeleteSeedKey_EvictsExactKeyOnly(t *testing.T) {
	svc := New(nil, nil)
	svc.seedKeys["k1"] = &KeyInfo{Key: "k1", Name: "a"}
	svc.seedKeys["k10"] = &KeyInfo{Key: "k10", Name: "b"}
	svc.setCache("k1", svc.seedKeys["k1"])
	svc.setCache("k10", svc.seedKeys["k10"])
	svc.setCache("other", &KeyInfo{Key: "other"})

	if ok := svc.DeleteSeedKey("k1"); !ok {
		t.Fatal("expected delete success")
	}

	if _, ok := svc.cache["k1"]; ok {
		t.Fatal("deleted key must be evicted from local cache")
	}
	// 删除 k1 不能连带清除 key 名前缀相同的其他条目（曾为 HasPrefix 前缀匹配缺陷）。
	if _, ok := svc.cache["k10"]; !ok {
		t.Fatal("key with the deleted key as a name prefix must be retained")
	}
	// 无关条目同样必须保留，确认只清除了目标条目。
	if _, ok := svc.cache["other"]; !ok {
		t.Fatal("unrelated cache entry must be retained")
	}
	if _, ok := svc.seedKeys["k10"]; !ok {
		t.Fatal("other seed key must be retained")
	}
}

func TestDeleteSeedKey_RemovesRedis(t *testing.T) {
	mr, rdb := newMiniRedisClient(t)
	// 使用 nil seeds 构造，避免异步同步 goroutine 与断言产生数据竞争；
	// 再通过 CreateSeedKey 加入种子 Key（修复后 CreateSeedKey 会正确落库，无需补偿写入）。
	svc := New(rdb, nil)
	svc.CreateSeedKey("k1", "first")

	if ok := svc.DeleteSeedKey("k1"); !ok {
		t.Fatal("expected delete success")
	}
	if mr.Exists(apikeyPrefix + "k1") {
		t.Fatal("deleted key must be removed from redis")
	}
}

func TestDeleteSeedKey_RedisError(t *testing.T) {
	rdb := newBrokenRedisClient(t)
	svc := New(rdb, nil)
	svc.CreateSeedKey("k1", "first")
	if ok := svc.DeleteSeedKey("k1"); !ok {
		t.Fatal("delete must succeed even if redis del fails")
	}
}

func TestSyncSeedKeysToRedis_Success(t *testing.T) {
	mr, rdb := newMiniRedisClient(t)
	// 使用 nil seeds 构造，随后直接调用，避免并发 goroutine 干扰。
	svc := New(rdb, nil)
	svc.seedKeys = seeds(map[string]string{"k1": "n1", "k2": "n2"})

	svc.syncSeedKeysToRedis()

	// 两个种子 Key 都必须落库且无 TTL（原实现收尾 Expire(key,0) 会删除它们，见 DEBT.md 2.3）。
	for _, key := range []string{"k1", "k2"} {
		if !mr.Exists(apikeyPrefix + key) {
			t.Fatalf("seed key %q must persist in redis", key)
		}
		if ttl := mr.TTL(apikeyPrefix + key); ttl != 0 {
			t.Fatalf("seed key %q expected no expiry, got TTL %v", key, ttl)
		}
	}
}

func TestSyncSeedKeysToRedis_Error(t *testing.T) {
	rdb := newBrokenRedisClient(t)
	svc := New(rdb, nil)
	svc.seedKeys = seeds(map[string]string{"k1": "n1"})
	// 不应 panic，错误仅记录告警。
	svc.syncSeedKeysToRedis()
}

func TestValidate_Concurrent(t *testing.T) {
	svc := New(nil, seeds(map[string]string{"seed-1": "client-1"}))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := svc.Validate("seed-1"); !ok {
				t.Error("expected concurrent validation success")
			}
		}()
	}
	wg.Wait()
}
