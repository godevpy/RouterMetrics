# merlin_exporter Makefile
# 用于 Asuswrt-Merlin 路由器的轻量级 Prometheus Exporter 构建配置

BINARY_NAME   ?= merlin_exporter
MODULE        := metrics/cmd/merlin_exporter
BIN_DIR       := bin
DIST_DIR      := dist

# 版本信息（自动从 git 标签或短哈希中提取）
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "1.0.0")
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# 纯静态编译参数：剔除符号表以压缩体积，注入版本变量
LDFLAGS       := -s -w \
                 -X main.Version=$(VERSION) \
                 -X main.Commit=$(COMMIT) \
                 -X main.BuildTime=$(BUILD_TIME)

# UPX 工具探测
UPX           ?= $(shell command -v upx 2>/dev/null)

.PHONY: all build test clean release-arm64 release-armv7 release-amd64 release help

all: test build

help:
	@echo "merlin_exporter 构建命令说明:"
	@echo "  make build          - 编译当前平台本地二进制 (开发调试)"
	@echo "  make test           - 运行全量单元测试与竞态检测"
	@echo "  make release-arm64  - 静态交叉编译 Linux arm64 二进制 (RT-AX86U / GT-AX6000 等)"
	@echo "  make release-armv7  - 静态交叉编译 Linux armv7 二进制 (RT-AC86U / RT-AX56U 等)"
	@echo "  make release-amd64  - 静态交叉编译 Linux amd64 二进制 (Ubuntu / x86 软路由)"
	@echo "  make release        - 构建全架构二进制并打包至 $(DIST_DIR)/"
	@echo "  make clean          - 清理构建产物与临时文件"

# 本地调试构建
build:
	@mkdir -p $(BIN_DIR)
	@echo "==> 正在编译本地开发版本 ($(VERSION))..."
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/merlin_exporter
	@echo "==> 编译完成: $(BIN_DIR)/$(BINARY_NAME)"

# 全量测试
test:
	@echo "==> 运行全量单元测试与数据竞争检查..."
	go test -v -race ./...
	@echo "==> 代码静态检查 (go vet)..."
	go vet ./...

# 交叉编译: Linux ARM64 (aarch64)
release-arm64:
	@mkdir -p $(BIN_DIR)
	@echo "==> 正在静态交叉编译 Linux arm64 二进制..."
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)_arm64 ./cmd/merlin_exporter
	@if [ -n "$(UPX)" ]; then \
		echo "==> 正在使用 UPX 压缩 $(BIN_DIR)/$(BINARY_NAME)_arm64..."; \
		$(UPX) --best --lzma $(BIN_DIR)/$(BINARY_NAME)_arm64; \
	else \
		echo " [提示] 未检测到 upx 命令，已跳过二进制压缩（可在 Ubuntu 上安装 upx-ucl 后重新构建以节省空间）。"; \
	fi
	@ls -lh $(BIN_DIR)/$(BINARY_NAME)_arm64

# 交叉编译: Linux ARMv7 (armv7l, 32位)
release-armv7:
	@mkdir -p $(BIN_DIR)
	@echo "==> 正在静态交叉编译 Linux armv7 二进制..."
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)_armv7 ./cmd/merlin_exporter
	@if [ -n "$(UPX)" ]; then \
		echo "==> 正在使用 UPX 压缩 $(BIN_DIR)/$(BINARY_NAME)_armv7..."; \
		$(UPX) --best --lzma $(BIN_DIR)/$(BINARY_NAME)_armv7; \
	else \
		echo " [提示] 未检测到 upx 命令，已跳过二进制压缩。"; \
	fi
	@ls -lh $(BIN_DIR)/$(BINARY_NAME)_armv7

# 编译: Linux AMD64 (x86_64, Ubuntu软路由测试环境)
release-amd64:
	@mkdir -p $(BIN_DIR)
	@echo "==> 正在静态交叉编译 Linux amd64 二进制..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)_amd64 ./cmd/merlin_exporter
	@if [ -n "$(UPX)" ]; then \
		echo "==> 正在使用 UPX 压缩 $(BIN_DIR)/$(BINARY_NAME)_amd64..."; \
		$(UPX) --best --lzma $(BIN_DIR)/$(BINARY_NAME)_amd64; \
	else \
		echo " [提示] 未检测到 upx 命令，已跳过二进制压缩。"; \
	fi
	@ls -lh $(BIN_DIR)/$(BINARY_NAME)_amd64

# 打包发布全套归档
release: release-arm64 release-armv7 release-amd64
	@mkdir -p $(DIST_DIR)
	@echo "==> 正在生成发布压缩包..."
	@# arm64 tar.gz
	@tar -czf $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-linux-arm64.tar.gz \
		-C $(BIN_DIR) $(BINARY_NAME)_arm64 \
		-C ../scripts services-start.sample \
		-C ../ README.md 2>/dev/null || \
		tar -czf $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-linux-arm64.tar.gz -C $(BIN_DIR) $(BINARY_NAME)_arm64
	@# armv7 tar.gz
	@tar -czf $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-linux-armv7.tar.gz \
		-C $(BIN_DIR) $(BINARY_NAME)_armv7 2>/dev/null || true
	@# amd64 tar.gz
	@tar -czf $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-linux-amd64.tar.gz \
		-C $(BIN_DIR) $(BINARY_NAME)_amd64 2>/dev/null || true
	@echo "==> 发布包生成成功:"
	@ls -lh $(DIST_DIR)

# 清理构建临时产物
clean:
	@echo "==> 正在清理构建产物..."
	@rm -rf $(BIN_DIR) $(DIST_DIR)
	@echo "==> 清理完成。"
