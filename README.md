# LLM Gateway

大模型 API 网关，支持多 Provider 路由、协议自动转换（OpenAI ↔ Anthropic）、Token 用量统计与持久化、熔断降级、限流与内置管理后台。

## 核心特性

- **多 Provider 路由** — 支持 OpenAI / Anthropic / DeepSeek / 商汤日日新 / 小米 等多个上游，按 priority / round_robin / latency_optimized / cost_optimized 策略自动选路
- **协议转换** — 客户端使用 OpenAI 格式，自动转换为 Anthropic 格式发往上游，反之亦然（非流式 + SSE 流式）
- **模型 Tier 分级** — 虚拟模型名支持 `premium / standard / economy` 分级，路由时自动按级匹配和降级 fallback
- **Token 估算 & 用量持久化** — 基于 `tiktoken-go` 本地估算，同时获取上游真实用量，写入 PostgreSQL（支持文件降级）
- **熔断保护** — 基于 `gobreaker` 的熔断器，Provider 异常时自动降级到其他候选
- **API Key 管理** — 支持配置文件种子 Key + Redis 动态 Key，三层缓存验证
- **内置管理后台** — Web 管理界面 + REST API，支持 JWT / Basic Auth 认证，动态管理 Provider、API Key、模型配置
- **SSE 流式转发** — 实时转发并重写 model 字段，兼容客户端 SDK

## 架构概览

```mermaid
flowchart TB
    subgraph Clients
        OpenAI_SDK[OpenAI SDK]
        Anthropic_SDK[Anthropic SDK]
    end

    subgraph Gateway[LLM Gateway]
        direction TB
        GatewayCore[路由 & 协议转换]
        Admin[管理后台 /admin]
    end

    subgraph Backends[后端 LLM 服务]
        OpenAI_Chat[OpenAI]
        Anthropic_API[Anthropic API]
        DeepSeek[DeepSeek]
        ShangTang[商汤]
        XiaoMi[小米]
    end

    subgraph Storage[存储层]
        PostgreSQL[(PostgreSQL)]
        FileStorage[(文件存储)]
        Redis[(Redis)]
    end

    OpenAI_SDK -->|OpenAI 格式| GatewayCore
    Anthropic_SDK -->|Anthropic 格式| GatewayCore

    GatewayCore -->|OpenAI 格式| OpenAI_Chat
    GatewayCore -->|Anthropic 格式| Anthropic_API
    GatewayCore -->|OpenAI/Anthropic 格式| DeepSeek
    GatewayCore -->|OpenAI/Anthropic 格式| ShangTang
    GatewayCore -->|OpenAI/Anthropic 格式| XiaoMi

    Admin --> PostgreSQL
    Admin --> Redis

    GatewayCore -.->|缓存读取| Redis
    GatewayCore -.->|降级写入| FileStorage
    GatewayCore -->|持久化| PostgreSQL
```

### 请求生命周期

```
客户端请求 → Auth 中间件(验证 API Key) → RateLimit 中间件
  → Handler(解析协议: OpenAI/Anthropic)
    → Mapper(模型名 allowlist 检查)
    → Token Service(估算输入 tokens)
    → Router(选择上游 Provider + 模型)
      → Protocol Resolver(格式转换 + 发送请求)
        → [非流式] 解析上游响应 → Token Service(记录用量) → 返回客户端
        → [流 式]  SSE 转发 + 累计 content → Token Service(记录用量)
```

## 技术栈

| 组件 | 库 |
|---|---|
| Web 框架 | [Gin](https://github.com/gin-gonic/gin) |
| 配置管理 | [Viper](https://github.com/spf13/viper) + YAML 序列化 |
| PostgreSQL | [pgx/v5](https://github.com/jackc/pgx) |
| Redis | [go-redis](https://github.com/redis/go-redis) |
| Token 估算 | [tiktoken-go](https://github.com/pkoukk/tiktoken-go) |
| 熔断器 | [gobreaker](https://github.com/sony/gobreaker) |
| 管理后台认证 | [golang-jwt/jwt/v5](https://github.com/golang-jwt/jwt) |
| 日志 | [zerolog](https://github.com/rs/zerolog) |

## 快速开始

### 前置条件

- Go 1.25+
- Docker & Docker Compose（可选，用于 PostgreSQL / Redis）
- 各 Provider 的 API Key

### 1. 配置环境变量

复制 `.env.example` 为 `.env` 并填入你的 API Key：

```bash
cp .env.example .env
# 编辑 .env
```

`.env` 文件格式：

```bash
OPENAI_API_KEY=sk-xxx
ANTHROPIC_API_KEY=sk-ant-xxx
DEEPSEEK_API_KEY=sk-xxx
SENSENOVA_API_KEY=sk-xxx
XIAOMI_TP_API_KEY=tp-xxx
GLM_API_KEY=xxx
NVIDIA_API_KEY=xxx
REDIS_PASSWORD=password
POSTGRES_PASSWORD=password
ADMIN_PASSWORD=your_admin_password_here
ADMIN_JWT_SECRET=your_admin_jwt_secret_here
```

### 2. 启动依赖服务

```bash
# 启动 PostgreSQL + Redis
make docker-up-db
```

### 3. 运行网关

```bash
# 开发模式（热重载，需安装 air）
make dev

# 或直接运行
make run
```

### 4. 验证

```bash
# 健康检查
curl http://localhost:8080/health

# OpenAI 兼容接口
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-gateway-dev-key-001" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# Anthropic 兼容接口
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-gateway-dev-key-001" \
  -d '{
    "model": "claude",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024
  }'

# 打开管理后台
open http://localhost:8080/admin
```

## 配置指南

所有配置集中在 `configs/config.yaml`，通过环境变量覆盖。

### 基础配置

```yaml
app:
  env: "dev"          # dev | prod
  port: 8080

log:
  level: "debug"      # debug | info | warn | error
  format: "console"   # json | console
```

### Provider 配置

支持任意 Provider（每个 Provider 可配置为 OpenAI 或 Anthropic 协议）：

```yaml
providers:
  openai:
    base_url: "https://api.openai.com/v1"
    api_key: "${OPENAI_API_KEY}"
    timeout: 300s
    protocol: "openai"      # 上游使用的协议
  anthropic:
    base_url: "https://api.anthropic.com/v1"
    api_key: "${ANTHROPIC_API_KEY}"
    timeout: 300s
    protocol: "anthropic"
  deepseek_openai:
    base_url: "https://api.deepseek.com"
    api_key: "${DEEPSEEK_API_KEY}"
    timeout: 300s
    protocol: "openai"
```

### 模型路由配置

虚拟模型（对外暴露） → 真实模型（上游）的映射：

```yaml
# 对外暴露的虚拟模型，带 tier 分级
models:
  - name: "gpt-4"       # 客户端请求用的模型名
    tier: "premium"
  - name: "claude"
    tier: "premium"
  - name: "deepseek"
    tier: "economy"

# 真实的 fallback 链（路由目标）
real_models:
  strategy: "priority"  # priority | round_robin | latency_optimized | cost_optimized
  models:
    - provider: "anthropic"
      model: "claude-sonnet-4-20250514"
      weight: 70
      timeout: 300s
      cost: 3.0
      tier: "premium"
    - provider: "openai"
      model: "gpt-5"
      weight: 50
      timeout: 300s
      cost: 2.5
      tier: "premium"
    - provider: "seneenova"
      model: "deepseek-v4-flash"
      weight: 80
      timeout: 300s
      cost: 0.03
      tier: "economy"
    - provider: "openai"
      model: "gpt-4-turbo"
      weight: 1
      timeout: 300s
      cost: 1.0
      tier: "premium"
      disabled: true   # 设为 true 后路由时跳过此条目，保留配置不删除
```

#### 路由策略

| 策略 | 说明 |
|---|---|
| `priority` | 按配置顺序依次尝试（默认） |
| `round_robin` | 加权轮询，按 weight 分发 |
| `latency_optimized` | 选择历史延迟最低的 Provider |
| `cost_optimized` | 选择成本最低的 Provider |

#### Tier 分级机制

当虚拟模型配置了 `tier` 时（如 `gpt-4: premium`），路由时只选择带相同 tier 或未指定 tier 的 fallback 候选，实现分级保障：

```
gpt-4 (premium)  →  {anthropic/claude-sonnet-4-20250514 (premium),
                      openai/gpt-5 (premium)}  ← 跳过 economy 级
deepseek (economy) →  {seneenova/deepseek-v4-flash (economy),
                       xiaomi/mimo-v2.5-pro (standard)}  ← 也可以包含无 tier 通用候选
```

#### 禁用模型

通过 `disabled: true` 禁用某个 real_model 条目，该条目会被路由完全跳过（不参与任何路由策略），但配置保留在 YAML 中不被删除。适用于需要临时下线某个上游、又希望快速恢复的场景。

在管理后台的 real_models 列表中可以看到每个条目的「状态」列（启用 / 禁用），编辑时勾选「禁用」复选框即可切换。

### 熔断器

```yaml
circuit_breaker:
  max_requests: 3        # 半开状态允许试探请求数
  interval: 10s          # 统计周期
  timeout: 5s            # 请求超时
  failure_threshold: 5   # 连续失败次数触发熔断
  cooldown: 30s          # 熔断后冷却时间
```

### 限流

```yaml
rate_limit:
  enabled: true
  requests_per_second: 100
  burst: 150
```

### Token 估算

```yaml
token:
  tokenizer_mapping:
    "gpt-4o-2024-11-20": "cl100k_base"
    "claude-3-5-sonnet-20241022": "claude"
    "deepseek-chat": "deepseek"
```

### API Key 配置

```yaml
api_keys:
  - key: "sk-gateway-dev-key-001"
    name: "dev-client-1"
  - key: "sk-gateway-dev-key-002"
    name: "dev-client-2"

# 跳过认证的路径（支持 /* 前缀匹配）
auth_whitelist:
  - "/health"
  - "/admin/*"
```

### 管理员配置

```yaml
admin:
  password: "${ADMIN_PASSWORD}"      # 管理员登录密码（支持 ${ENV_VAR} 引用）
  jwt_secret: "${ADMIN_JWT_SECRET}"  # JWT 签名密钥（为空时自动生成 256-bit 随机密钥）
  token_expiry: 24h                  # JWT token 有效期
```

## API 接口

### 代理接口（转发到上游 LLM）

| 端点 | 协议 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI 格式 | 聊天补全（非流式 / SSE 流式） |
| `POST /v1/completions` | OpenAI 格式 | 补全（兼容） |
| `POST /v1/messages` | Anthropic 格式 | Anthropic Messages API 代理 |
| `POST /v1/messages/count_tokens` | Anthropic 格式 | 计数 tokens |
| `GET /v1/models` | OpenAI 格式 | 列出可用模型 |

### 管理员查询接口（无需认证）

| 端点 | 说明 |
|---|---|
| `GET /admin/usage` | 时间范围内总 Token 数 |
| `GET /admin/usage/daily` | 每日聚合统计 |
| `GET /admin/usage/stats` | 总请求数 / Token 数统计 |
| `GET /admin/usage/by-real-model` | 按真实模型 (real_model) 汇总 Token 统计 |
| `GET /admin/usage/by-api-key` | 按 API Key + 时间粒度查询用量统计 |
| `GET /admin/calibration` | 本地估算 vs 上游真实用量的校准比例 |
| `GET /admin/breakers` | 查看所有熔断器状态 |

### 管理后台 API（需 JWT / Basic Auth 认证）

| 端点 | 方法 | 说明 |
|---|---|---|
| `POST /admin/login` | POST | 管理员登录，返回 JWT token |
| `GET /admin/api-keys` | GET | 列出所有 API Key |
| `POST /admin/api-keys` | POST | 新增 API Key |
| `DELETE /admin/api-keys/:key` | DELETE | 删除 API Key |
| `GET /admin/api-keys/:key/usage` | GET | 查询指定 API Key 的用量统计 |
| `GET /admin/providers` | GET | 列出所有 Provider 熔断器状态 |
| `GET /admin/providers/config` | GET | 列出所有 Provider 配置（隐藏 API Key） |
| `POST /admin/providers` | POST | 新增 Provider |
| `PUT /admin/providers/:name` | PUT | 更新 Provider 配置 |
| `DELETE /admin/providers/:name` | DELETE | 删除 Provider |
| `GET /admin/models` | GET | 列出所有虚拟模型 |
| `POST /admin/models` | POST | 新增虚拟模型 |
| `DELETE /admin/models/:name` | DELETE | 删除虚拟模型 |
| `GET /admin/real-models` | GET | 列出所有真实模型（real_models） |
| `POST /admin/real-models` | POST | 新增 real_model 条目 |
| `PUT /admin/real-models/:index` | PUT | 更新 real_model 条目 |
| `DELETE /admin/real-models/:index` | DELETE | 删除 real_model 条目 |
| `PATCH /admin/real-models/strategy` | PATCH | 更新路由策略 |
| `GET /admin/config` | GET | 查看运行中配置摘要 |
| `GET /admin` | GET | 管理后台 Web UI（SPA） |
| `GET /admin/assets/*` | GET | 静态资源文件 |

> **认证方式**: 管理后台 API 支持两种认证方式：
> 1. **JWT Bearer**（推荐）— `POST /admin/login` 获取 token，后续请求使用 `Authorization: Bearer <token>`
> 2. **Basic Auth** — `Authorization: Basic base64("admin:<password>")`（需配置 `admin.password`）

### 通用

| 端点 | 说明 |
|---|---|
| `GET /health` | 健康检查 |

## Token 用量持久化

网关支持三种存储后端，按优先级自动降级：

1. **PostgreSQL**（首选）— 自动建表，SQL 聚合查询，高并发
2. **Redis** — 基于 List，带容量上限（10K/API Key, 100K 全局）
3. **文件存储**（最终降级）— `data/usage.json`，开发调试用

启动时自动检测 PostgreSQL 可用性，连接失败则降级到文件存储。

### PostgreSQL 表结构

```sql
CREATE TABLE usage_records (
    id              BIGSERIAL PRIMARY KEY,
    request_id      VARCHAR(255) NOT NULL UNIQUE,
    virtual_model   VARCHAR(255) NOT NULL,
    real_model      VARCHAR(255) NOT NULL,
    provider        VARCHAR(255) NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    est_input       INTEGER NOT NULL DEFAULT 0,
    est_output      INTEGER NOT NULL DEFAULT 0,
    official_in     INTEGER NOT NULL DEFAULT 0,
    official_out    INTEGER NOT NULL DEFAULT 0,
    api_key         VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
```

## 管理后台

网关内置 Web 管理界面，支持浏览器直接访问 `http://localhost:8080/admin`。

### 登录方式

1. 配置 `ADMIN_PASSWORD` 环境变量
2. 打开 `http://localhost:8080/admin`
3. 在登录页面输入密码，获取 JWT token（自动存储到 localStorage）
4. 后续 API 请求自动携带 Bearer token

### 管理能力

- **Provider 管理** — 新增/编辑/删除上游 Provider，实时生效
- **API Key 管理** — 新增/删除访问密钥，同步至 Redis
- **模型配置管理** — 管理虚拟模型和 real_model 列表，切换路由策略，支持按条目禁用/启用
- **用量监控** — 按 API Key / 模型 / 时间粒度查询 Token 用量
- **熔断器状态** — 实时查看各 Provider 熔断器状态

## 项目结构

```
llm-gateway/
├── cmd/gateway/
│   ├── main.go              # 入口：初始化、依赖注入、路由注册
│   └── handlers.go          # HTTP 处理器：代理转发、用量查询、管理接口
├── internal/
│   ├── auth/
│   │   └── service.go       # API Key 验证（本地缓存 + 种子 Key + Redis）
│   ├── config/
│   │   └── config.go        # 配置结构体定义与加载（Viper + YAML 序列化）
│   ├── health/
│   │   └── health.go        # 健康检查端点
│   ├── mapper/
│   │   └── mapper.go        # 模型名 allowlist + 响应 model 字段重写
│   ├── middleware/
│   │   ├── adminauth.go     # 管理后台 JWT / Basic Auth 认证中间件
│   │   ├── auth.go          # API Key 认证中间件（Bearer / x-api-key）
│   │   ├── cors.go          # CORS 中间件
│   │   ├── logger.go        # 请求日志中间件
│   │   ├── ratelimit.go     # 按 API Key 限流（golang.org/x/time/rate）
│   │   └── recovery.go      # Panic 恢复中间件
│   ├── protocol/
│   │   ├── types.go         # 请求/响应类型定义
│   │   └── protocol.go      # 协议转换 + 上游请求调度（4 种组合）
│   ├── provider/
│   │   ├── provider.go          # Provider 抽象 + HTTP 发送
│   │   └── anthropic_converter.go  # Anthropic ↔ OpenAI 格式转换
│   ├── router/
│   │   └── router.go        # 路由选择 + 熔断器 + 梯次策略
│   ├── storage/
│   │   ├── usage.go         # UsageStorage 接口 + FileStorage + RedisStorage
│   │   └── postgres.go      # PostgresStorage 实现（pgx）
│   ├── stream/
│   │   ├── stream.go        # SSE 流式转发 + 重写 + Token 提取
│   │   └── anthropic_sse.go # Anthropic SSE ↔ OpenAI SSE 转换
│   └── token/
│       └── token.go         # Token 估算 + 记录 + 校准
├── pkg/
│   ├── breaker/
│   │   └── breaker.go       # 熔断器封装
│   ├── ratelimit/
│   │   └── ratelimit.go     # 限流器封装
│   ├── redis/
│   │   └── client.go        # Redis 客户端工厂
│   └── tokenizer/
│       └── tokenizer.go     # tiktoken 估算工具
├── web/
│   └── static/
│       ├── index.html       # 管理后台 SPA 入口
│       ├── css/
│       │   └── dashboard.css
│       └── js/
│           └── dashboard.js
├── configs/
│   ├── config.yaml          # 开发环境配置
│   └── config.prod.yaml     # 生产环境配置覆盖
├── deployments/
│   ├── docker/
│   │   ├── Dockerfile
│   │   ├── docker-compose.yml
│   │   └── prometheus.yml   # Prometheus 配置（保留兼容，但网关不再暴露 /metrics）
│   └── k8s/
│       ├── deployment.yaml
│       └── secret.yaml
├── migrations/
│   └── 001_create_usage_records.sql
└── data/
    └── usage.json           # 文件存储降级方案（自动生成）
```

## Docker 部署

```bash
# 构建镜像
make docker

# 启动完整服务栈（gateway + postgres + redis）
make docker-run

# 仅启动数据库
make docker-up-db

# 停止
make docker-down
```

网关在 Docker Compose 中运行在 `prod` 模式，自动读取项目根目录的 `.env` 文件配置。

> **注意**：Docker Compose 中仍包含 Prometheus 容器（端口 9091），但网关已不再暴露 `/metrics` 端点。如需监控请自行配置或移除 Prometheus 服务。

## 协议转换矩阵

网关自动处理 4 种客户端 ↔ 上游协议组合：

| 客户端协议 | 上游协议 | 非流式 | 流式 (SSE) |
|---|---|---|---|
| OpenAI | OpenAI | 直接转发 | 直接转发（重写 model 字段） |
| OpenAI | Anthropic | 消息格式转换 + 响应转换 | `OpenAIStreamConverter` 包装 |
| Anthropic | OpenAI | 消息格式转换 + 响应转换 | `AnthropicSSEConverter` 包装 |
| Anthropic | Anthropic | 直接转发 | 直接转发（含规范化） |

## 开发

```bash
# 安装依赖
make deps

# 运行测试
make test

# 热重载开发
make dev

# 代码格式化
make fmt

# Lint
make lint

# 跨平台构建
make package
```

## Makefile 命令速查

| 命令 | 说明 |
|---|---|
| `build` | 本地编译 |
| `run` | 直接运行 |
| `dev` | 热重载开发（air） |
| `test` | 运行测试 |
| `docker` | 构建 Docker 镜像 |
| `docker-run` | Docker Compose 启动全部服务 |
| `docker-down` | 停止所有服务 |
| `docker-up-db` | 启动 PostgreSQL + Redis |
| `docker-up-redis` | 仅启动 Redis |
| `docker-up-postgres` | 仅启动 PostgreSQL |
| `package` | 跨平台打包（darwin/linux/windows，含 web 资源） |
| `fmt` | 代码格式化 |
| `lint` | Lint 检查 |
| `deps` | 依赖管理 |