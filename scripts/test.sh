#!/bin/bash

set -e

BASE_URL="http://localhost:8080"
API_KEY="sk-test"

echo "=== LLM Gateway 测试脚本 ==="

# 1. 健康检查
echo "[1/5] 健康检查..."
curl -s "${BASE_URL}/health" | jq .

# 2. 列出模型
echo "[2/5] 列出模型..."
curl -s "${BASE_URL}/v1/models" | jq .

# 3. 非流式请求
echo "[3/5] 非流式聊天..."
curl -s "${BASE_URL}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${API_KEY}" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello, how are you?"}],
    "max_tokens": 100
  }' | jq .

# 4. 流式请求
echo "[4/5] 流式聊天..."
curl -s "${BASE_URL}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${API_KEY}" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'

# 5. Prometheus 指标
echo "[5/5] 指标..."
curl -s "${BASE_URL}/metrics" | head -20

echo "=== 测试完成 ==="
