.PHONY: build run test clean docker docker-run docker-down docker-up-redis darwin-amd64 darwin-arm64 linux-amd64 package

APP_NAME := llm-gateway
BUILD_DIR := ./build
GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
DOCKER_COMPOSE := docker-compose -f deployments/docker/docker-compose.yml

# 本地编译
build:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="-s -w" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/gateway

# macOS Intel (amd64)
darwin-amd64:
	@mkdir -p $(BUILD_DIR)/darwin-amd64
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" \
		-o $(BUILD_DIR)/darwin-amd64/$(APP_NAME) ./cmd/gateway
	@cp -r configs $(BUILD_DIR)/darwin-amd64/

# macOS Apple Silicon (arm64)
darwin-arm64:
	@mkdir -p $(BUILD_DIR)/darwin-arm64
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" \
		-o $(BUILD_DIR)/darwin-arm64/$(APP_NAME) ./cmd/gateway
	@cp -r configs $(BUILD_DIR)/darwin-arm64/

# Linux x86_64
linux-amd64:
	@mkdir -p $(BUILD_DIR)/linux-amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" \
		-o $(BUILD_DIR)/linux-amd64/$(APP_NAME) ./cmd/gateway
	@cp -r configs $(BUILD_DIR)/linux-amd64/

# 打包所有平台 + 包含 .env 到每个压缩包
package: darwin-amd64 darwin-arm64 linux-amd64
	@mkdir -p $(BUILD_DIR)
	@for plat in darwin-amd64 darwin-arm64 linux-amd64; do \
		STAGE=$$(mktemp -d); \
		cp -r $(BUILD_DIR)/$$plat/* $$STAGE/; \
		if [ -f .env ]; then cp .env $$STAGE/; fi; \
		tar czf $(BUILD_DIR)/$(APP_NAME)-$$plat.tar.gz -C $$STAGE .; \
		rm -rf $$STAGE; \
	done

run:
	go run ./cmd/gateway

dev:
	air -c .air.toml

test:
	go test -v -race ./...

clean:
	rm -rf $(BUILD_DIR)

docker:
	docker build -t $(APP_NAME):latest -f deployments/docker/Dockerfile .

docker-run:
	$(DOCKER_COMPOSE) up -d

docker-down:
	$(DOCKER_COMPOSE) down

docker-up-redis:
	$(DOCKER_COMPOSE) up -d redis

fmt:
	go fmt ./...

lint:
	golangci-lint run ./...

deps:
	go mod tidy
	go mod download
