# LLM Gateway

大模型网关，支持模型自动切换、Token 计数、模型名映射。

## 技术栈

- **Web 框架**: Gin
- **配置管理**: Viper
- **缓存**: Redis (go-redis)
- **Token 计算**: tiktoken-go
- **日志**: zerolog
- **监控**: Prometheus
- **熔断**: gobreaker

## 快速开始

```bash
# 1. 安装依赖
go mod download

# 2. 启动 Redis (本地开发)
docker-compose -f deployments/docker/docker-compose.yml up -d redis

# 3. 运行
make run

# 4. 测试
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-test" \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}'
```

## 项目结构

```
llm-gateway/
├── cmd/gateway/          # 入口
├── internal/             # 内部代码
│   ├── config/           # 配置
│   ├── middleware/       # 中间件
│   ├── router/           # 路由 & 熔断
│   ├── mapper/           # 模型名映射
│   ├── token/            # Token 计算 & 官网同步
│   ├── provider/         # 上游 Provider 客户端
│   ├── stream/           # SSE 流处理
│   ├── metrics/          # Prometheus 指标
│   └── health/           # 健康检查
├── pkg/                  # 可复用包
│   ├── tokenizer/        # Token 计算工具
│   ├── breaker/          # 熔断器封装
│   └── ratelimit/        # 限流器
├── configs/              # 配置文件
├── deployments/          # 部署配置
└── api/                  # API 定义
```

## 配置说明

见 `configs/config.yaml`
