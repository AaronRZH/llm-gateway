# llm-gateway 技术债治理清单

> **最后度量**: 2026-09-23（合并 PR #7-#14，刷新复杂度基线）  
> **适用范围**: 生产 Go 代码（排除 `*_test.go` 与 `.tmp/`）  
> **治理原则**: 先锁定行为，再小步重构；优先级由业务风险、变更频率、测试保护和复杂度共同决定

---

## 1. 度量口径与基线

### 1.1 口径

- **源文件数**：生产 `.go` 文件数量，不含测试文件。
- **函数数**：生产代码中的函数与方法声明数量。
- **代码行（LOC）**：非空且不是整行 `//` 注释的行数；不是 `wc -l` 的物理行数。
- **圈复杂度**：按 gocyclo 规则计算，即基础路径加 `if`、循环、`case`、通信分支及 `&&` / `||` 等决策点。
- **函数行数**：从函数声明起始行到结束行，包含空行和注释。
- **测试覆盖率**：使用 Go statement coverage；它只表示语句执行情况，不等同于业务场景、并发或安全行为已得到充分验证。

覆盖率复现命令：

```bash
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

后续应把 LOC、复杂度和重复度检查脚本固化到仓库或 CI，避免不同工具口径漂移。

### 1.2 当前基线

| 指标 | 当前值 |
|------|--------|
| 生产 Go 文件数 | 28 |
| 函数/方法数 | 405 |
| 代码行 | 8646 |
| 平均函数复杂度 | 4.34 |
| 最大函数复杂度 | 19（`main` / `AnthropicSSEConverter.convert` / `completeContentBlock`） |
| 最大函数行数 | 193（`main`） |
| 测试文件数 | 26 |
| 总语句覆盖率 | 61.3% |

复杂度最高的函数：

| 函数 | 位置 | 复杂度 | 行数 |
|------|------|--------|------|
| `main` | `cmd/gateway/main.go` | 19 | 193 |
| `AnthropicSSEConverter.convert` | `internal/stream/anthropic_sse.go` | 19 | 92 |
| `completeContentBlock` | `internal/provider/anthropic_converter.go` | 19 | 43 |
| `AdminAuth` | `internal/middleware/adminauth.go` | 18 | 70 |
| `rewriteXMLToolCallsChecked` | `cmd/gateway/handlers.go` | 17 | 58 |
| `extractUsage` | `internal/stream/stream.go` | 16 | 63 |
| `handleAnthropicMessages` | `cmd/gateway/handlers.go` | 15 | 121 |
| `ConvertAnthropicToOpenAIResponse` | `internal/provider/anthropic_converter.go` | 15 | 102 |
| `scanAndForward` | `internal/stream/stream.go` | 15 | 75 |

> 复杂度基线已从阶段 1 的最高值 48 降至 19。`RewriteAndForwardWithToolRepair`（原 C=48）、`ConvertAnthropicMessagesToOpenAI`（原 C=23）、`handleAnthropicMessages`（原 C=28）、`handleChatCompletion`（原 C=25）、`handleCountTokens`（原 C=20）、`ConvertResponse`（原 C=20）、`ConvertResponseWithModel`（原 C=19）、`Normalize`（原 C=20）与 `OpenAIStreamConverter.convert`（原 C=20）均已拆分，不再进入前 9 名。剩余热点集中在 `main`（C=19）、`AnthropicSSEConverter.convert`（C=19）与 `completeContentBlock`（C=19）。

重点包覆盖率：

| 包 | 覆盖率 | 说明 |
|----|--------|------|
| `cmd/gateway` | 34.1% | 两个核心 handler 已有 characterization tests，但分支覆盖不足 |
| `internal/auth` | 98.0% | 已建立完整测试（种子 Key、缓存、Redis 回退） |
| `internal/protocol` | 88.5% | `Resolve` 拆分后覆盖 97.6% |
| `internal/provider` | 56.1% | 转换主路径覆盖提升，`ConvertAnthropicMessagesToOpenAI` 达成 100% |
| `internal/storage` | 62.7% | RedisStorage 补充测试 + postgres 纯函数测试，`RedisStorage` 0% → 80%，`postgres.go` 0% → 60% |
| `internal/stream` | 81.4% | 已有流式和工具调用修复测试 |
| `internal/middleware` | 93.7% | `AdminAuth` 已有 JWT、Basic Auth 等测试 |
| `internal/health` | 100.0% | 已补全测试 |
| `internal/mapper` | 81.2% | 模型映射逻辑 |
| `internal/router` | 89.9% | 路由链与候选选择 |
| `internal/token` | 58.7% | 用量统计与估算 |
| `pkg/redis` | 100.0% | 已补全测试 |
| `pkg/breaker` | 75.0% | 熔断器 |
| `pkg/ratelimit` | 31.6% | 限流器，覆盖不足 |
| `pkg/tokenizer` | 67.9% | 分词与 token 估算 |

### 1.3 治理进展（阶段 1 / P0 行为锁定）

> 最近更新：2026-09-23。本小节记录相对 1.2 基线的变更，1.2 保留为历史基线。

已完成 P0「先建立安全网」，新增测试，并修复审核阶段发现的 2 个缺陷：

| 范围 | 新增测试文件 | 覆盖率变化 |
|------|--------------|-----------|
| `internal/auth` | `service_test.go` | 0.0% → 98.1% |
| `internal/protocol`（`Resolve`） | `resolve_test.go` | 包 29.8% → 87.9%，`Resolve` 0.0% → 97.6% |
| `cmd/gateway`（两个核心 handler） | `handlers_characterization_test.go` | 包 6.7% → 35.2%；`handleChatCompletion` 0% → 81.1%，`handleAnthropicMessages` 0% → 75.0% |
| `internal/health` | `health_test.go` | 0.0% → 100% |
| `pkg/redis` | `client_test.go` | 0.0% → 100% |
| `internal/stream` | `stream_test.go`（新增 chunk JSON 合法性校验） | 包 76.5% → 80.1% |
| `internal/provider`（`ConvertAnthropicMessagesToOpenAI`） | `anthropic_converter_test.go`（新增 9 个 tests） | 包 43.7% → 56.1%，`ConvertAnthropicMessagesToOpenAI` 0% → 100% |

**总语句覆盖率：39.8% → 56.3%**（阶段 1 完成时）。

验证命令（全部通过）：`go test ./...`、`go test -race ./...`、`go vet ./...`。

- 新增测试依赖：`github.com/alicebob/miniredis/v2`（进程内 Redis，测试不依赖外部服务）。
- `internal/auth` 覆盖种子 Key、本地缓存命中/过期、Redis 命中/失败回退、并发校验等场景。
- `protocol.Resolve` 覆盖 4 种协议组合 × 流式/非流式、429 退避语义、5xx 错误、连接失败与转换失败兜底。
- 两个核心 handler 覆盖主要成功路径与错误路径（模型未放行、非法 JSON、无可用候选、上游错误透传、429 退避、流式终止）。

**测试期间发现的行为特征与缺陷（P0 阶段）**：

1. `internal/auth/service.go` 的 `CreateSeedKey` 与 `syncSeedKeysToRedis` 结尾调用 `pipe.Expire(ctx, key, 0)`。在 Redis 语义下 0 TTL 会立即删除刚写入的 key（源码注释声称"永不过期"，实为缺陷）。**影响**：种子 Key 同步到 Redis 后随即被删除，`auth` 的 Redis 回退路径失效——多副本部署下，副本 A 通过 `CreateSeedKey` 创建的 key 在副本 B 无法认证。**本缺陷已在阶段 1 修复**：删除两处 `pipe.Expire(ctx, key, 0)` 调用（而非改用 `Persist`——key 是 `HSET` 新建的，从未有过 TTL，`PERSIST` 是冗余 no-op），`TestCreateSeedKey_SyncsToRedis` 从锁定缺陷行为改为断言 `Exists == true` 且无 TTL，并追加 `syncSeedKeysToRedis` 双 Key 落库断言。

2. `DeleteSeedKey` 原用 `strings.HasPrefix(k, key)` 清除本地缓存，删除 `"k1"` 会连带清除 `"k10"`、`"k11"` 等 key 名前缀相同的条目。**本缺陷已在阶段 1 修复**（改为 `delete(s.cache, key)` 精确匹配），由 `TestDeleteSeedKey_EvictsExactKeyOnly` 锁定。注：该测试初版断言方向写反（`if ok` 而非 `if !ok`），在缺陷存在时未失败，导致一次假通过；已修正为 `!ok`。

3. **新发现（本阶段审核，已修复）**：`internal/stream/anthropic_sse.go` 的 `OpenAIStreamConverter.writeChunkPlain` 生成不合法 JSON。模板为 `..."choices":[{"index":0,` + jsonBody + `}]}`，而 7 个调用方返回的 `jsonBody` 形如 `{"delta":...}` 等，拼接结果 `{"index":0,{"delta":...}}` 缺少 `"delta":` 键（`{"index":0,{` 两个相邻 `{` 中间无逗号，不合法）。**影响范围**：`OpenAIStreamConverter` 生成的所有 chunk——`writeDelta`、`writeTextDelta`、`writeToolStart`、`writeToolArgsDelta`、`writeToolFinal`、`writeTextFinal`、`writeUsage` 全部受影响，文本路径与工具路径都产出损坏输出。导致 **Case 2（Anthropic 上游 → OpenAI 客户端）的流式 SSE chunk 全部为非法 JSON**，OpenAI 客户端无法解析。该缺陷自 `36b7864 unified provider sse reverse` 引入，现有 `internal/stream` 测试只做子串匹配、无 `json.Valid` 校验因而未暴露。**已在本阶段修复**：模板改为 `"choices":[{"index":0,` + `jsonBody[1:]` + `]}`——剥离 jsonBody 首个 `{`，由 jsonBody 自带的尾部 `}` 闭合 choice 对象。回归测试见 `TestOpenAIStreamConverter_AllChunkJSONValid`（覆盖 text 与 tool 两条路径）及 `resolve_test.go` 的 `assertSSEJSONValid`。

   注：本缺陷的首版记录错误地排除了 `writeToolStart`/`writeToolFinal`，经实测验证二者同样受影响，已更正。

### 1.4 治理进展（阶段 2 / P1 复杂度降低与缺陷修复）

> 最近更新：2026-09-23。本小节记录阶段 1 完成后的增量变更，1.3 保留为阶段 1 快照。

| 范围 | 完成方式 | 结果 |
|------|----------|------|
| `internal/provider`（`ConvertAnthropicMessagesToOpenAI`） | 拆为 4 个职责单一函数，新增 9 个 characterization tests | C=23 → 15（达成 ≤15 目标），覆盖率 0% → 100% |
| `internal/storage`（`RedisStorage`） | 新增 9 个 tests（`miniredis` 驱动），修复 `summarizeRecordsByRealModel` 空指针 bug | 包 29.6% → 62.7%，`RedisStorage` 0% → 80% |
| `internal/storage`（postgres 纯函数） | 新增 `postgres_test.go`（`buildDSN`、`parseTimeRange`、`FileStorage.compact`/`AdminDailyStats`） | `buildDSN` 0% → 100%，`parseTimeRange` 0% → 100%，`compact` 0% → 100% |
| `cmd/gateway`（`handleAnthropicMessages`） | 拆为 4 个函数（`resolveAnthropicCandidate`、`forwardUpstreamErrorIfPresent`、`forwardStreamResponse`、`forwardNonStreamResponse`），8 个 characterization tests 保持通过 | C=28 → 15（达成 ≤15 目标），222 → 121 行 |
| `cmd/gateway`（`handleChatCompletion`） | 拆为 4 个函数（`resolveOpenAICandidate`、`forwardOpenAIStreamResponse`、`forwardOpenAINonStreamResponse`、`selectChatStreamCandidate`），10 个 characterization tests 保持通过 | C=25 → 14（达成 ≤15 目标），219 → 112 行 |
| `cmd/gateway`（`handleCountTokens`） | 拆为 2 个函数（`resolveCountTokensTarget`、`proxyCountTokensResponse`），消除两条 CountTokens 路径的重复调用/解析/转发逻辑（~60 行 → 1 个函数），既有测试保持通过 | C=20 → <12（跌破 metrics 显示阈值），113 → 33 行 |
| `internal/provider`（`ConvertResponse` + `ConvertResponseWithModel`） | 提取 2 个纯函数（`filterAnthropicContent`、`buildAnthropicUsage`），消除两个方法间约 50 行重复的 content 过滤和 usage 提取逻辑，既有测试保持通过 | C=20/19 → <12（均跌破 metrics 显示阈值），86/80 → 46/42 行 |
| `internal/toolcall`（`Normalize`） | 提取 3 个纯函数（`matchFamilyTag`、`findToolCallEnd`、`buildToolCallEntry`），将 tag 匹配、结束位置查找、条目构建从主循环中分离，既有测试保持通过 | C=20 → <12（跌破 metrics 显示阈值），92 → 58 行 |
| `internal/stream`（`OpenAIStreamConverter.convert`） | 提取 `processLine` 方法封装循环体（空行跳过、注释保活转发、payload 解析、7 类事件分发），`convert` 仅保留 scanner 设置、循环调度和收尾，既有测试保持通过 | C=20 → <12（跌破 metrics 显示阈值），79 → 33 行 |

**总语句覆盖率：56.3% → 61.3%**。

**新发现的缺陷（已修复）**：`RedisStorage.summarizeRecordsByRealModel` 创建 bucket 后未把指针赋给循环变量 `b`，首条记录即触发 nil pointer panic。同文件其他三个分桶方法（`summarizeRecordsDaily/Weekly/Monthly`）均有 `b = buckets[key]`，唯独此处缺失——典型的复制粘贴遗留 bug。

---

## 2. P0：先建立安全网

P0 表示当前缺少足够回归保护，继续修改可能造成认证、协议兼容或核心请求链路回归。P0 阶段以测试和行为固化为主，不进行大规模结构调整。

### 2.1 `internal/auth/service.go`：安全敏感模块零覆盖

**现状**

- 249 个物理行，包覆盖率 0%。
- 同时负责种子 Key、本地缓存、Redis 查询与同步。
- 存在异步同步和共享状态，需要覆盖并发与失败场景。

**行动**

- 新增 `internal/auth/service_test.go`。
- 覆盖种子 Key 验证、创建/删除、缓存命中与过期、Redis 命中/失败回退。
- 使用 `miniredis` 或小接口 mock，避免依赖真实 Redis。
- 在 CI 中使用 `go test -race ./...` 验证并发路径。

**完成标准**

- `internal/auth` 语句覆盖率达到 80% 以上。
- 关键成功、失败、过期和并发场景均有断言。
- 测试不依赖外部 Redis 服务。

### 2.2 核心 handler 与 `protocol.Resolve`：高复杂度、主路径零覆盖

**现状**

- `handleChatCompletion`：C=47、346 行、函数覆盖率 0%。**已完成（阶段 2）**：拆为 4 个函数，C=47 → 14，346 → 112 行，10 个 characterization tests 全部通过；见 3.1。
- `handleAnthropicMessages`：C=48、314 行、函数覆盖率 0%。**已完成（阶段 2）**：拆为 4 个函数，C=28 → 15，222 → 121 行，8 个 characterization tests 全部通过；见 3.1。
- `protocol.Resolve`：C=36、259 行、函数覆盖率 0%。**已完成（refactor/resolve）**：拆为薄分发器 + 4 个 case handler，C=36 → 10，覆盖率 0% → 88.5%。
- 现有 `handlers_test.go` 主要覆盖工具调用辅助函数，没有锁定完整请求行为。

**行动**

- 先增加表驱动的 characterization tests，再拆分函数。
- 覆盖 OpenAI/Anthropic 请求、流式/非流式、provider fallback、超时、工具调用修复及上游错误。
- 对 `Resolve` 覆盖不同 provider 协议、工具定义、消息转换和非法输入。
- 通过 `httptest`、fake provider 和可控 stream reader 隔离外部依赖。

**完成标准**

- 两个核心 handler 的主要成功路径和错误路径均有测试。
- `Resolve` 语句覆盖率达到 80% 以上。
- 后续重构前后 characterization tests 输出保持一致。

### 2.3 `internal/auth` 种子 Key 无法落 Redis：多副本认证不一致

**现状（已修复）**

- 原实现：`CreateSeedKey`（`service.go:104`）与 `syncSeedKeysToRedis`（`service.go:237`）在 `HSet` 后执行 `pipe.Expire(ctx, key, 0)`，注释声称「永不过期」。
- 按 Redis 语义，`EXPIRE key 0` 等价于立即 `DEL`（而非忽略）。因此每次写入后 key 随即被删除。
- 结果：`checkRedis` 中 `EXISTS gateway:apikeys:<key>` 恒为 0，**Redis 回退分支永远走不到**——`Validate` 实际只依赖本地 `seedKeys`。
- 多副本部署下，配置中未声明的 Key（如副本 A 运行时 `CreateSeedKey` 创建的 Key）在副本 B 无法认证。这是认证不一致问题，属 P0。

**修复（已完成）**

- 直接删除两处 `pipe.Expire(ctx, key, 0)` 调用，同时删除 `syncSeedKeysToRedis` 中错误的「永不过期」注释。
- 说明：子代理审查曾建议改用 `pipe.Persist(ctx, key)`，但 `PERSIST` 在 Redis 语义上是「移除已存在的 TTL」。本例中 key 是 `HSET` 新建的，从未有过 TTL，故 `PERSIST` 是冗余 no-op，反而引入了不必要的一次网络往返与语义混淆；直接删除 `Expire` 行才是正确、最小的修复。
- 原 `TestCreateSeedKey_SyncsToRedis` 以断言 `Exists == false` 锁定的是**缺陷行为**；已改为断言 `Exists == true` 且 `TTL == 0`（无过期），使其成为修复正确性的回归测试。
- 追加 `TestSyncSeedKeysToRedis_Success` 的双 Key 落库断言，把 `syncSeedKeysToRedis` 路径也纳入真实验证。

**完成标准（已满足）**

- 创建与同步后的种子 Key 在 Redis 中存在（`EXISTS == true`），无 TTL。
- `checkRedis` 的 `EXISTS` 分支可被真实命中，Redis 回退路径首次获得有效覆盖。
- 全仓 `go test ./...` 通过。

---

## 3. P1：降低核心请求链路复杂度

### 3.1 `cmd/gateway/handlers.go`：职责过度集中

**现状**

- 2171 LOC、82 个函数。
- 同时包含用户请求、协议适配、工具调用修复、用量统计和管理后台处理逻辑。
- 主要风险不是单纯文件过长，而是核心请求编排与多种细节逻辑共同变化。
- **进展**：两个核心 handler 均已达成 ≤15 复杂度目标——`handleChatCompletion` C=14、112 行；`handleAnthropicMessages` C=15、121 行。`handleCountTokens` 亦已拆分（C=20 → <12、113 → 33 行）。编排行数略超 100 行目标（特殊说明：含大量 error 处理和注释，实际逻辑行数约 80）。

**行动**

- 将公开 API handler 与管理后台 handler 分文件组织；优先保持在同一 package 内，避免一次性改变 API 和依赖方向。
- 将 `handleChatCompletion` 和 `handleAnthropicMessages` 拆成"解析与校验、路由与调用、响应与记账"三个阶段。
- 复用 `internal/protocol`、`internal/stream`、`internal/toolcall` 已有职责，避免再创建一套转换逻辑。
- 只有在依赖边界稳定后，再评估是否迁移到 `internal/handler` package。

**完成标准**

- ✅ 两个核心 handler 圈复杂度分别降到 15 以下。
- ⚠️ 编排函数控制在约 100 行以内（实际 112/121 行，略超；特殊说明：含大量 error 处理和注释，实际逻辑行数约 80）。
- ✅ 不改变现有 HTTP 状态码、响应格式、fallback 和用量记录行为（18 个 characterization tests 全部通过）。

### 3.2 `internal/stream/stream.go`：转发、缓存和工具修复耦合

**现状**

- 582 LOC、16 个函数。
- `RewriteAndForwardWithToolRepair`：C=48、227 行。
- 包覆盖率 76.5%，已有 `stream_test.go`、`wrapper_stream_test.go` 和 `clarify_xml_test.go`，不需要重新创建测试文件。

**行动**

- 从现有函数中提取无副作用的 chunk 解析与转换逻辑。
- 分离输出缓冲、工具调用校验/修复和 SSE 写入职责。
- 扩充现有测试，重点覆盖 repair provider 失败、客户端断开、异常结束、多工具调用和 usage 合并。
- 每次只迁移一个职责，并保持当前流式时序和终止事件不变。

**完成标准**

- `RewriteAndForwardWithToolRepair` 仅保留编排逻辑，复杂度降到 15 以下。
- `internal/stream` 覆盖率不低于当前 76.5%，目标达到 85%。
- OpenAI 与 Anthropic 的 SSE 终止、错误和工具调用事件均有测试。

### 3.3 `internal/stream/anthropic_sse.go`：两个方向的转换器均复杂

**现状**

- `OpenAIStreamConverter.convert`：C=39、163 行，负责 Anthropic → OpenAI。**已完成（refactor/openai-converter-events）**：拆为 5 个事件处理器，C=39 → 20、163 → 79 行（详见 `b503f95`）。**进一步拆分**：提取 `processLine` 方法封装循环体，C=20 → <12、79 → 33 行。
- `AnthropicSSEConverter.convert`：C=33、194 行，负责 OpenAI → Anthropic。**已完成（refactor/anthropic-sse-convert）**：拆为 4 个事件处理器 + 1 个 tool 状态结构体，C=33 → 19、194 → 92 行。
- 两个方法同名，讨论和度量时必须带接收者名称。

**行动**

- 分别提取事件解析、状态迁移和事件输出，不把两个方向强行合并为一个通用转换器。
- 把纯转换做成可直接表驱动测试的函数；I/O 层只负责扫描、写入和关闭。
- 补齐多 tool block、非法 JSON、ping、上游中断和客户端断开测试。

**完成标准**

- 两个 `convert` 方法复杂度均降到 15 以下。
- 状态迁移和协议输出可在不启动 goroutine 的情况下单测。
- 保留现有 idle timeout 和恰好一次终止事件语义。

### 3.4 `internal/protocol` 与 `internal/provider`：转换边界重叠

**现状**

- `protocol.Resolve`：C=36、259 行。**已完成（refactor/resolve）**：拆为薄分发器 + 4 个 case handler，C=36 → 10，覆盖率 0% → 88.5%。
- `ContentToBlocks`：C=31、83 行。**已完成**：降至 C=13、43 行。
- `ConvertAnthropicMessagesToOpenAI`：C=23、100 行。**已完成（refactor/convert-anthropic-messages）**：拆为 4 个职责单一函数，C=23 → 15，覆盖率 0% → 100%。
- `ConvertResponse` / `ConvertResponseWithModel`：C=20 / C=19。**已完成**：提取 `filterAnthropicContent` 与 `buildAnthropicUsage` 两个纯函数，均降至 C<12。
- `Normalize` 属于 `internal/toolcall`，不属于 provider 债务。

**行动**

- 先明确 `protocol` 负责请求编排、`provider` 负责供应商格式转换、`toolcall` 负责工具调用规范化。
- 将 provider 转换测试迁移或新增到 provider package 对应测试文件中。
- 在边界明确后拆分 `Resolve`、`ContentToBlocks` 和消息转换函数。
- 避免创建语义重复的 `converter.go`；按请求/响应或协议方向命名文件。

**完成标准**

- 三个 package 的职责说明写入 package 文档或代码注释。
- 上述高复杂度函数降到 15 以下。
- 转换规则在所属 package 内有表驱动测试，跨包测试只验证集成行为。

---

## 4. P2：存储、启动和基础设施整理

### 4.1 `internal/storage`：优先验证后端一致性

**现状（进展中）**

- 覆盖率已从 29.6% 提升至 62.7%（目标 70%）：RedisStorage 补充测试（`miniredis` 驱动），postgres 纯函数补测，`FileStorage.compact`/`AdminDailyStats` 补齐。
- 剩余缺口集中在 `PostgresStorage` 的数据库交互方法（`Persist`、`queryRecords`、`aggregateByTimeUnit` 等 21 个函数均为 0%），需要真实 PostgreSQL 或 mock。
- 存储逻辑会影响计费与统计准确性，风险主要来自不同后端行为不一致。

**行动**

- 为 `UsageStorage` 定义共享 contract tests，复用于可用的存储实现。
- 覆盖时间边界、模型过滤、重复 request ID、聚合粒度和空结果语义。
- 使用可控测试容器或接口 mock 覆盖 PostgreSQL；使用 `miniredis` 覆盖 Redis。
- 在测试保护建立后，再按“文件存储、Redis 存储、聚合辅助函数”拆分文件；不以文件行数本身作为拆分目标。

**完成标准**

- 各存储实现通过同一组契约测试。
- 核心统计结果在不同后端保持一致。
- `internal/storage` 覆盖率达到 70% 以上，并明确未覆盖的外部集成路径。
- **当前进度**：62.7% / 70%。`FileStorage` 与 `RedisStorage` 已接近全覆盖；剩余缺口为 `PostgresStorage` 的数据库交互路径，需引入测试容器或 pgx mock。

### 4.2 `cmd/gateway/main.go`：启动流程难测试

**现状**

- `main`：C=19、193 行。
- 混合配置加载、依赖构建、路由注册、服务启动和优雅关闭。

**行动**

- 提取 `buildApplication` 或等价 bootstrap 函数，显式返回依赖与清理函数。
- 提取 HTTP router 构建函数，使用 `httptest` 验证路由和中间件注册。
- 注意与现有 `internal/router`（模型路由）区分命名，避免概念混淆。

**完成标准**

- `main` 只保留配置加载、启动和信号处理。
- 启动依赖构建及 HTTP 路由可独立测试。
- 资源关闭顺序和失败处理有测试或明确断言。

### 4.3 小型零覆盖包

**范围**

- `internal/health`：17 行，覆盖率 0%。
- `pkg/redis`：31 行，覆盖率 0%。

**行动与完成标准**

- 为 health handler 增加状态码和响应体测试。
- 为 Redis 配置映射增加测试；连接行为由 auth/storage 集成测试覆盖。
- 不因文件短小而过度抽象。

### 4.4 `AdminAuth`：保持测试，按需拆分

`AdminAuth` 虽然 C=18，但已有 JWT、Basic Auth、OPTIONS 和错误路径测试，所属包覆盖率 93.7%。暂不因复杂度数字单独拆包；若新增角色或权限模型，再提取 token 解析与授权策略。

---

## 5. 增量质量门禁

以下规则针对新增或实质修改的代码。存量问题用本清单逐步偿还，不要求单个 PR 一次达到全仓目标。

1. **复杂度**：新增函数目标 ≤15；超过时需拆分，或在 PR 中记录不能拆分的理由。
2. **函数长度**：新增函数目标 ≤100 行；测试表、声明式配置和机械生成代码可例外。
3. **覆盖率**：新增/变更代码覆盖率目标 ≥80%；同时不得无理由降低相关 package 的既有覆盖率。
4. **核心行为**：认证、协议转换、流式终止、计费统计的修改必须包含成功与失败路径测试。
5. **PR 大小**：建议净变更 ≤400 行；测试、生成代码、格式化和经说明的机械迁移不纳入硬性限制。
6. **重复代码**：重复检测只作为审查信号；忽略 import、声明、生成代码和常见样板，不能仅凭“连续 6 行相同”阻止合并。
7. **公共 API**：对外可导出 API 或配置格式发生不兼容变化时，必须写迁移说明；只有仓库实际采用 CHANGELOG 后才要求更新。

建议 CI 最低检查：

```bash
go test ./...
go test -race ./...
go vet ./...
```

复杂度、diff coverage 和重复度检查应在选定并固化工具后再设为强制门禁。

---

## 6. 所有权与变更记录

- 当前仓库没有有效的 `CODEOWNERS`，不在本文件中使用 `@username` 占位符冒充 owner。
- `internal/*` 是模块内部实现，不称为对外公共模块，也不单独标注虚构的 `v1.0.0`。
- 如团队需要强制审查人，应新增真实的 `.github/CODEOWNERS`，并由团队确认人员或小组。
- 如需要维护发布记录，应在仓库根目录建立统一 `CHANGELOG.md`；在建立前不把它列为合并前置条件。

---

## 7. 执行节奏

### 阶段 1：行为锁定（1–2 周）

- 完成 `internal/auth` 测试。
- 为两个核心 handler 和 `protocol.Resolve` 增加 characterization tests。
- 将覆盖率命令接入 CI，记录 package 基线。

### 阶段 2：核心链路小步重构（2–4 周）

- 拆分两个核心 handler。
- 拆分 `RewriteAndForwardWithToolRepair`。
- 分别拆分两个 SSE converter。
- 每个 PR 只迁移一个职责，并由现有行为测试兜底。

### 阶段 3：边界与后端一致性（2–3 周）

- 明确 protocol/provider/toolcall 边界并整理测试归属。
- 建立存储 contract tests，覆盖 Redis/PostgreSQL 路径。
- 提取可测试的 bootstrap 与 HTTP router 构建逻辑。

### 阶段 4：持续治理

- 每月记录复杂度、package 覆盖率、变更热点、回滚和线上缺陷。
- 优先处理“高变更频率 + 低覆盖率 + 高业务影响”的模块。
- 指标用于发现风险，不把单一数字当作重构目标。

---

## 8. AI 辅助边界

AI 适合协助生成 characterization tests、执行小范围机械迁移、检查重复和整理文档；架构与公共 API 变更仍需由代码 owner 审核。AI 生成的代码必须通过同样的测试、静态检查和人工审查，不设置与人工代码不同的质量标准。
