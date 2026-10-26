MODULE := github.com/gitlayzer/fastgpt-sync-adapter
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v2.0.0")
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -w -s \
	-X '$(MODULE)/internal/version.Version=$(VERSION)' \
	-X '$(MODULE)/internal/version.GitCommit=$(GIT_COMMIT)' \
	-X '$(MODULE)/internal/version.BuildDate=$(BUILD_DATE)'

BIN_DIR := bin
ADAPTER_BIN := $(BIN_DIR)/adapter

.PHONY: all build build-nocgo test test-coverage vet clean docker-build run help

all: build ## 默认构建主服务 (含 cgo)

help: ## 显示帮助信息
	@printf "\033[36m%-20s\033[0m %s\n" "目标" "说明"
	@printf "\033[36m%-20s\033[0m %s\n" "----" "----"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## 使用 CGO 编译主程序 (需目标架构 CGO 环境与企微动态库)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o $(ADAPTER_BIN) ./cmd/adapter
	@echo "==> 构建成功: $(ADAPTER_BIN)"

build-nocgo: ## 纯 Go 编译主程序 (无 CGO，可跨平台交叉编译，不启用企微 C-SDK)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(ADAPTER_BIN) ./cmd/adapter
	@echo "==> 构建成功 (纯 Go): $(ADAPTER_BIN)"

test: ## 运行所有单元测试
	go test -v ./internal/...

test-coverage: ## 运行单元测试并生成覆盖率报告
	go test -coverprofile=coverage.out ./internal/...
	go tool cover -html=coverage.out -o coverage.html
	@echo "==> 覆盖率报告已生成: coverage.html"

vet: ## 代码静态检查
	go vet ./...

clean: ## 清理二进制产物与临时文件
	rm -rf $(BIN_DIR)/adapter coverage.out coverage.html
	@echo "==> 清理完成"

docker-build: ## 构建 Docker 镜像
	docker build -t fastgpt-sync-adapter:$(VERSION) -t fastgpt-sync-adapter:latest .
	@echo "==> Docker 镜像构建完成: fastgpt-sync-adapter:$(VERSION)"

run: build-nocgo ## 本地启动主服务 (读取根目录 config.yaml)
	./$(ADAPTER_BIN) --config config.yaml
