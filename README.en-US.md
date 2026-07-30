

# LLM Gateway

LLM API Gateway supporting multi-provider routing, automatic protocol conversion (OpenAI ↔ Anthropic), token usage statistics & persistence, circuit breaking & fallback, rate limiting, and a built-in admin dashboard.

## Core Features

- **Multi-Provider Routing** — Supports multiple upstream providers such as OpenAI / Anthropic / DeepSeek / SenseTime SenseNova / Xiaomi, with automatic routing based on `priority` / `round_robin` / `latency_optimized` / `cost_optimized` strategies
- **Protocol Conversion** — Automatically converts between OpenAI and Anthropic formats for upstream requests and client responses (non-streaming + SSE streaming)
- **Model Tiering** — Virtual model names support `premium` / `standard` / `economy` tiers, with automatic tier-based matching and fallback during routing
- **Token Estimation & Usage Persistence** — Locally estimates tokens using `tiktoken-go`, retrieves actual upstream usage, and persists data to PostgreSQL (with fallback to file storage)
- **Circuit Breaking** — Uses `gobreaker` for circuit breakers, automatically failing over to other candidates when a provider encounters errors
- **API Key Management** — Supports seed keys from config files and dynamic keys in Redis, with a three-layer cache validation mechanism
- **Built-in Admin Dashboard** — Web UI + REST API supporting JWT / Basic Auth for dynamic management of providers, API keys, and model configurations
- **SSE Streaming Forwarding** — Real-time forwarding with model field rewriting, fully compatible with client SDKs

## Architecture Overview

```mermaid
flowchart TB
    subgraph Clients
        OpenAI_SDK[OpenAI SDK]
        Anthropic_SDK[Anthropic SDK]
    end

    subgraph Gateway[LLM Gateway]
        direction TB
        GatewayCore[Routing & Protocol Conversion]
        Admin[Admin Dashboard /admin]
    end

    subgraph Backends[Backend LLM Services]
        OpenAI_Chat[OpenAI]
        Anthropic_API[Anthropic API]
        DeepSeek[DeepSeek]
        ShangTang[SenseTime]
        XiaoMi[Xiaomi]
    end

    subgraph Storage[Storage Layer]
        PostgreSQL[(PostgreSQL)]
        FileStorage[(File Storage)]
        Redis[(Redis)]
    end

    OpenAI_SDK -->|OpenAI Format| GatewayCore
    Anthropic_SDK -->|Anthropic Format| GatewayCore

    GatewayCore -->|OpenAI Format| OpenAI_Chat
    GatewayCore -->|Anthropic Format| Anthropic_API
    GatewayCore -->|OpenAI/Anthropic Format| DeepSeek
    GatewayCore -->|OpenAI/Anthropic Format| ShangTang
    GatewayCore -->|OpenAI/Anthropic Format| XiaoMi

    Admin --> PostgreSQL
    Admin --> Redis

    GatewayCore -.->|Cache Read| Redis
    GatewayCore -.->|Fallback Write| FileStorage
    GatewayCore -->|Persistence| PostgreSQL
```

### Request Lifecycle

```
Client Request → Auth Middleware (Validate API Key) → RateLimit Middleware
  → Handler (Parse Protocol: OpenAI/Anthropic)
    → Mapper (Model Name Allowlist Check)
    → Token Service (Estimate Input Tokens)
    → Router (Select Upstream Provider + Model)
      → Protocol Resolver (Format Conversion + Send Request)
        → [Non-Streaming] Parse Upstream Response → Token Service (Record Usage) → Return to Client
        → [Streaming] SSE Forwarding + Accumulate Content → Token Service (Record Usage)
```

## Tech Stack

| Component | Library |
|---|---|
| Web Framework | [Gin](https://github.com/gin-gonic/gin) |
| Configuration | [Viper](https://github.com/spf13/viper) + YAML serialization |
| PostgreSQL | [pgx/v5](https://github.com/jackc/pgx) |
| Redis | [go-redis](https://github.com/redis/go-redis) |
| Token Estimation | [tiktoken-go](https://github.com/pkoukk/tiktoken-go) |
| Circuit Breaker | [gobreaker](https://github.com/sony/gobreaker) |
| Admin Auth | [golang-jwt/jwt/v5](https://github.com/golang-jwt/jwt) |
| Logging | [zerolog](https://github.com/rs/zerolog) |

## Quick Start

### Prerequisites

- Go 1.25+
- Docker & Docker Compose (optional, for PostgreSQL / Redis)
- API Keys for each Provider

### 1. Configure Environment Variables

Copy `.env.example` to `.env` and fill in your API keys:

```bash
cp .env.example .env
# Edit .env
```

`.env` file format:

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

### 2. Start Dependency Services

```bash
# Start PostgreSQL + Redis
make docker-up-db
```

### 3. Run the Gateway

```bash
# Development mode (hot reload, requires `air` installed)
make dev

# Or run directly
make run
```

### 4. Verify

```bash
# Health check
curl http://localhost:8080/health

# OpenAI-compatible endpoint
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-gateway-dev-key-001" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# Anthropic-compatible endpoint
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-gateway-dev-key-001" \
  -d '{
    "model": "claude",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024
  }'

# Open admin dashboard
open http://localhost:8080/admin
```

## Configuration Guide

All configurations are centralized in `configs/config.yaml` and can be overridden via environment variables.

### Base Configuration

```yaml
app:
  env: "dev"          # dev | prod
  port: 8080
  # Overall request budget: total timeout for a single request (including all fallback candidates).
  # If candidates remain unsuccessful after this time, the request is terminated and an error is returned, preventing prolonged accumulation of N× upstream timeouts.
  # Set to 0 for no limit (falls back to each provider's timeout / server write_timeout).
  request_timeout: 120s

log:
  level: "debug"      # debug | info | warn | error
  format: "console"   # json | console
```

### Provider Configuration

Supports any provider (each can be configured for OpenAI or Anthropic protocol):

```yaml
providers:
  openai:
    base_url: "https://api.openai.com/v1"
    api_key: "${OPENAI_API_KEY}"
    timeout: 300s
    protocol: "openai"      # Protocol used by the upstream
  anthropic:
    base_url: "https://api.anthropic.com/v1"
    api_key: "${ANTHROPIC_API_KEY}"
    timeout: 300s
    # First byte (response header) timeout: fails and triggers fallback if no upstream response headers are received within this time.
    # Default is 15s; set to 0 to use the global default. Can be increased based on upstream network conditions (e.g., 30s for slow upstreams).
    response_header_timeout: 15s
    protocol: "anthropic"
  deepseek_openai:
    base_url: "https://api.deepseek.com"
    api_key: "${DEEPSEEK_API_KEY}"
    timeout: 300s
    protocol: "openai"
```

### Model Routing Configuration

Mapping from virtual models (exposed externally) to real models (upstream):

```yaml
# Virtual models exposed externally, with tier classification
models:
  - name: "gpt-4"       # Model name used by client requests
    tier: "premium"
  - name: "claude"
    tier: "premium"
  - name: "deepseek"
    tier: "economy"

# Actual fallback chain (routing targets)
real_models:
  strategy: "priority"  # priority | round_robin | latency_optimized | cost_optimized
  models:
    - provider: "anthropic"
      model: "claude-sonnet-4-20250514"
      priority: 10        # Priority: lower values take precedence; same values maintain config order
      weight: 70
      timeout: 300s
      cost: 3.0
      tier: "premium"
    - provider: "openai"
      model: "gpt-5"
      priority: 20
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
      disabled: true   # When set to true, this entry is skipped during routing but the config is preserved
```

#### Routing Strategies

| Strategy | Description |
|---|---|
| `priority` | Attempts in ascending order of the explicit `priority` field (lower values take precedence); same priority maintains config order; falls back to config order if not set (default) |
| `round_robin` | Weighted round-robin distribution based on `weight` |
| `latency_optimized` | Selects the provider with the lowest historical latency |
| `cost_optimized` | Selects the provider with the lowest cost |

#### Tier Classification Mechanism

When a virtual model has a `tier` configured (e.g., `gpt-4: premium`), routing only selects fallback candidates with the same tier or no tier specified, ensuring tier-based guarantees:

```
gpt-4 (premium)  →  {anthropic/claude-sonnet-4-20250514 (premium),
                      openai/gpt-5 (premium)}  ← Skips economy tier
deepseek (economy) →  {seneenova/deepseek-v4-flash (economy),
                       xiaomi/mimo-v2.5-pro (standard)}  ← Can also include generic candidates without a tier
```

#### Disabling Models

Disable a `real_model` entry using `disabled: true`. The entry will be completely skipped by the router (not participating in any routing strategy), but the configuration remains in the YAML file for easy restoration. Suitable for temporarily taking down an upstream while preserving the config for quick recovery.

In the admin dashboard's `real_models` list, you can see the "Status" column (Enabled / Disabled) for each entry. Simply check the "Disabled" checkbox when editing to toggle it.

### Circuit Breaker

```yaml
circuit_breaker:
  max_requests: 3        # Max requests allowed in half-open state
  interval: 10s          # Statistical interval
  timeout: 5s            # Request timeout
  failure_threshold: 5   # Consecutive failures to trigger circuit breaker
  cooldown: 30s          # Cooldown period after circuit opens
```

### Rate Limiting

```yaml
rate_limit:
  enabled: true
  requests_per_second: 100
  burst: 150
```

### Token Estimation

```yaml
token:
  tokenizer_mapping:
    "gpt-4o-2024-11-20": "cl100k_base"
    "claude-3-5-sonnet-20241022": "claude"
    "deepseek-chat": "deepseek"
```

### API Key Configuration

```yaml
api_keys:
  - key: "sk-gateway-dev-key-001"
    name: "dev-client-1"
  - key: "sk-gateway-dev-key-002"
    name: "dev-client-2"

# Paths to skip authentication (supports `/*` prefix matching)
auth_whitelist:
  - "/health"
  - "/admin/*"
```

### Admin Configuration

```yaml
admin:
  password: "${ADMIN_PASSWORD}"      # Admin login password (supports `${ENV_VAR}` references)
  jwt_secret: "${ADMIN_JWT_SECRET}"  # JWT signing secret (automatically generates a 256-bit random key if empty)
  token_expiry: 24h                  # JWT token expiration
```

## API Endpoints

### Proxy Endpoints (Forward to Upstream LLM)

| Endpoint | Protocol | Description |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Format | Chat completion (non-streaming / SSE streaming) |
| `POST /v1/completions` | OpenAI Format | Completion (compatible) |
| `POST /v1/messages` | Anthropic Format | Anthropic Messages API proxy |
| `POST /v1/messages/count_tokens` | Anthropic Format | Count tokens |
| `GET /v1/models` | OpenAI Format | List available models |

### Admin Query Endpoints (No Authentication Required)

| Endpoint | Description |
|---|---|
| `GET /admin/usage` | Total token count within a time range |
| `GET /admin/usage/daily` | Daily aggregated statistics |
| `GET /admin/usage/stats` | Total requests / token count statistics |
| `GET /admin/usage/by-real-model` | Token statistics aggregated by real model (`real_model`) |
| `GET /admin/usage/by-api-key` | Query usage statistics by API key + time granularity |
| `GET /admin/calibration` | Calibration ratio between local estimation and actual upstream usage |
| `GET /admin/breakers` | View circuit breaker status for all providers |

### Admin Dashboard API (Requires JWT / Basic Auth)

| Endpoint | Method | Description |
|---|---|---|
| `POST /admin/login` | POST | Admin login, returns JWT token |
| `GET /admin/api-keys` | GET | List all API keys |
| `POST /admin/api-keys` | POST | Create a new API key |
| `DELETE /admin/api-keys/:key` | DELETE | Delete an API key |
| `GET /admin/api-keys/:key/usage` | GET | Query usage statistics for a specific API key |
| `GET /admin/providers` | GET | List circuit breaker status for all providers |
| `GET /admin/providers/config` | GET | List all provider configurations (API keys hidden) |
| `POST /admin/providers` | POST | Create a new provider |
| `PUT /admin/providers/:name` | PUT | Update provider configuration |
| `DELETE /admin/providers/:name` | DELETE | Delete a provider |
| `GET /admin/models` | GET | List all virtual models |
| `POST /admin/models` | POST | Create a new virtual model |
| `DELETE /admin/models/:name` | DELETE | Delete a virtual model |
| `GET /admin/real-models` | GET | List all real models (`real_models`) |
| `POST /admin/real-models` | POST | Create a new `real_model` entry |
| `PUT /admin/real-models/:index` | PUT | Update a `real_model` entry |
| `DELETE /admin/real-models/:index` | DELETE | Delete a `real_model` entry |
| `PATCH /admin/real-models/strategy` | PATCH | Update routing strategy |
| `GET /admin/config` | GET | View running configuration summary |
| `GET /admin` | GET | Admin Dashboard Web UI (SPA) |
| `GET /admin/assets/*` | GET | Static asset files |

> **Authentication Methods**: The admin dashboard API supports two authentication methods:
> 1. **JWT Bearer** (Recommended) — Obtain a token via `POST /admin/login`, then use `Authorization: Bearer <token>` for subsequent requests
> 2. **Basic Auth** — `Authorization: Basic base64("admin:<password>")` (requires `admin.password` to be configured)

### General

| Endpoint | Description |
|---|---|
| `GET /health` | Health check |

## Token Usage Persistence

The gateway supports three storage backends, automatically failing over based on priority:

1. **PostgreSQL** (Preferred) — Automatic table creation, SQL aggregation queries, high concurrency
2. **Redis** — List-based, with capacity limits (10K per API key, 100K global)
3. **File Storage** (Final Fallback) — `data/usage.json`, for development and debugging

Automatically detects PostgreSQL availability on startup; falls back to file storage if connection fails.

### PostgreSQL Table Structure

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

## Admin Dashboard

The gateway includes a built-in web admin interface, accessible directly in the browser at `http://localhost:8080/admin`.

### Login Method

1. Configure the `ADMIN_PASSWORD` environment variable
2. Open `http://localhost:8080/admin`
3. Enter the password on the login page to obtain a JWT token (automatically stored in `localStorage`)
4. Subsequent API requests automatically include the Bearer token

### Management Capabilities

- **Provider Management** — Add/edit/delete upstream providers, effective in real-time
- **API Key Management** — Add/delete access keys, synced to Redis
- **Model Configuration Management** — Manage virtual models and `real_model` lists, switch routing strategies, and support per-entry disable/enable
- **Usage Monitoring** — Query token usage by API key / model / time granularity
- **Circuit Breaker Status** — View real-time circuit breaker status for each provider

## Project Structure

```
llm-gateway/
├── cmd/gateway/
│   ├── main.go              # Entry point: initialization, dependency injection, route registration
│   └── handlers.go          # HTTP handlers: proxy forwarding, usage queries, admin endpoints
├── internal/
│   ├── auth/
│   │   └── service.go       # API Key validation (local cache + seed keys + Redis)
│   ├── config/
│   │   └── config.go        # Configuration struct definitions and loading (Viper + YAML serialization)
│   ├── health/
│   │   └── health.go        # Health check endpoints
│   ├── mapper/
│   │   └── mapper.go        # Model name allowlist + response model field rewriting
│   ├── middleware/
│   │   ├── adminauth.go     # Admin dashboard JWT / Basic Auth middleware
│   │   ├── auth.go          # API Key authentication middleware (Bearer / x-api-key)
│   │   ├── cors.go          # CORS middleware
│   │   ├── logger.go        # Request logging middleware
│   │   ├── ratelimit.go     # Rate limiting by API key (golang.org/x/time/rate)
│   │   └── recovery.go      # Panic recovery middleware
│   ├── protocol/
│   │   ├── types.go         # Request/response type definitions
│   │   └── protocol.go      # Protocol conversion + upstream request dispatching (4 combinations)
│   ├── provider/
│   │   ├── provider.go          # Provider abstraction + HTTP sending
│   │   └── anthropic_converter.go  # Anthropic ↔ OpenAI format conversion
│   ├── router/
│   │   └── router.go        # Route selection + circuit breaker + tiered strategy
│   ├── storage/
│   │   ├── usage.go         # UsageStorage interface + FileStorage + RedisStorage
│   │   └── postgres.go      # PostgresStorage implementation (pgx)
│   ├── stream/
│   │   ├── stream.go        # SSE streaming forwarding + rewriting + token extraction
│   │   └── anthropic_sse.go # Anthropic SSE ↔ OpenAI SSE conversion
│   └── token/
│       └── token.go         # Token estimation + recording + calibration
├── pkg/
│   ├── breaker/
│   │   └── breaker.go       # Circuit breaker wrapper
│   ├── ratelimit/
│   │   └── ratelimit.go     # Rate limiter wrapper
│   ├── redis/
│   │   └── client.go        # Redis client factory
│   └── tokenizer/
│       └── tokenizer.go     # tiktoken estimation utility
├── web/
│   └── static/
│       ├── index.html       # Admin dashboard SPA entry point
│       ├── css/
│       │   └── dashboard.css
│       └── js/
│           └── dashboard.js
├── configs/
│   ├── config.yaml          # Development environment configuration
│   └── config.prod.yaml     # Production environment configuration overrides
├── deployments/
│   ├── docker/
│   │   ├── Dockerfile
│   │   ├── docker-compose.yml
│   │   └── prometheus.yml   # Prometheus configuration (kept for compatibility, but the gateway no longer exposes /metrics)
│   └── k8s/
│       ├── deployment.yaml
│       └── secret.yaml
├── migrations/
│   └── 001_create_usage_records.sql
└── data/
    └── usage.json           # File storage fallback scheme (auto-generated)
```

## Docker Deployment

```bash
# Build image
make docker

# Start full service stack (gateway + postgres + redis)
make docker-run

# Start only the database
make docker-up-db

# Stop
make docker-down
```

The gateway runs in `prod` mode within Docker Compose, automatically reading the `.env` file configuration from the project root.

> **Note**: Docker Compose still includes the Prometheus container (port 9091), but the gateway no longer exposes the `/metrics` endpoint. Configure monitoring yourself or remove the Prometheus service if needed.

## Protocol Conversion Matrix

The gateway automatically handles 4 client ↔ upstream protocol combinations:

| Client Protocol | Upstream Protocol | Non-Streaming | Streaming (SSE) |
|---|---|---|---|
| OpenAI | OpenAI | Direct forwarding | Direct forwarding (rewrites `model` field) |
| OpenAI | Anthropic | Message format conversion + response conversion | Wrapped by `OpenAIStreamConverter` |
| Anthropic | OpenAI | Message format conversion + response conversion | Wrapped by `AnthropicSSEConverter` |
| Anthropic | Anthropic | Direct forwarding | Direct forwarding (includes normalization) |

## Development

```bash
# Install dependencies
make deps

# Run tests
make test

# Hot reload development
make dev

# Code formatting
make fmt

# Lint
make lint

# Cross-platform build
make package
```

## Makefile Command Cheat Sheet

| Command | Description |
|---|---|
| `build` | Local compilation |
| `run` | Run directly |
| `dev` | Hot reload development (`air`) |
| `test` | Run tests |
| `docker` | Build Docker image |
| `docker-run` | Start all services via Docker Compose |
| `docker-down` | Stop all services |
| `docker-up-db` | Start PostgreSQL + Redis |
| `docker-up-redis` | Start only Redis |
| `docker-up-postgres` | Start only PostgreSQL |
| `package` | Cross-platform packaging (darwin/linux/windows, including web assets) |
| `fmt` | Code formatting |
| `lint` | Lint check |
| `deps` | Dependency management |
