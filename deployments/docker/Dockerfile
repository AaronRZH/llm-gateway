# 构建阶段
FROM golang:1.22-alpine AS builder

WORKDIR /app

# 安装依赖
RUN apk add --no-cache git

# 复制依赖文件
COPY go.mod go.sum ./
RUN go mod download

# 复制源码
COPY . .

# 构建
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gateway ./cmd/gateway

# 运行阶段
FROM alpine:3.19

RUN apk add --no-cache ca-certificates

WORKDIR /app

# 复制二进制和配置
COPY --from=builder /app/gateway .
COPY --from=builder /app/configs ./configs

EXPOSE 8080 9090

ENTRYPOINT ["./gateway"]
