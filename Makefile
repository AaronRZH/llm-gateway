.PHONY: build run test clean docker

APP_NAME := llm-gateway
BUILD_DIR := ./build
GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')

build:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="-s -w" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/gateway

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
	docker-compose -f deployments/docker/docker-compose.yml up -d

docker-down:
	docker-compose -f deployments/docker/docker-compose.yml down

fmt:
	go fmt ./...

lint:
	golangci-lint run ./...

deps:
	go mod tidy
	go mod download
