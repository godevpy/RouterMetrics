# merlin_exporter 实现计划 (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建专为 Asuswrt-Merlin 固件路由器定制的极轻量 Prometheus Exporter（`merlin_exporter`），支持模块化 Collector 开关、硬件/无线动态探测、细粒度内网终端与心跳监控，并具备 arm64/armv7 纯静态编译与 UPX 压缩能力。

**Architecture:** 采用 Go 原生标准库 + `prometheus/client_golang` 核心库开发，按子系统划分 Collector（system, netdev, thermal, wireless, nvram, lan_client），利用聚合器实现带全局超时的并发抓取与错误隔离，彻底杜绝重型 CLI 框架依赖。

**Tech Stack:** Go 1.26+、`github.com/prometheus/client_golang`、Makefile、UPX、Shell（Asuswrt-Merlin `services-start`）。

## 全局约束与规范 (Global Constraints)
- **CGO 禁用**：`CGO_ENABLED=0`，必须保证无 libc 动态链接。
- **内存控制**：常驻内存控制在 5MB~8MB 内。
- **存储控制**：strip (`-s -w`) + UPX 压缩后二进制控制在 2.5MB~3.5MB。
- **命名规范**：所有暴露的指标统一使用 `router_` 前缀。
- **CLI 安全调用**：外部命令（`wl`、`nvram` 等）调用必须注入 `context.WithTimeout`，防止底层 Broadcom 驱动挂起抓取进程。
- **语言规范**：代码注释、文档与提交信息一律使用中文。

---

### Task 1: 初始化工程依赖与通用底层工具库 (`pkg/util` 与 `pkg/nvram`)

**Files:**
- Create: `pkg/util/exec.go`
- Create: `pkg/util/exec_test.go`
- Create: `pkg/nvram/nvram.go`
- Create: `pkg/nvram/nvram_test.go`
- Modify: `go.mod`

**Interfaces:**
- Produces:
  - `util.RunCommandWithTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error)`
  - `nvram.Client`: `Get(ctx context.Context, key string) (string, error)`
  - `nvram.Client`: `GetAll(ctx context.Context, keys []string) (map[string]string, error)`

- [ ] **Step 1: 引入 `client_golang` 依赖并更新 `go.mod`**

```bash
go get github.com/prometheus/client_golang@v1.20.5
go mod tidy
```

- [ ] **Step 2: 编写 `pkg/util` 的测试用例（测试超时控制与命令执行）**

在 `pkg/util/exec_test.go` 中编写：
```go
package util

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunCommandWithTimeout_Success(t *testing.T) {
	ctx := context.Background()
	out, err := RunCommandWithTimeout(ctx, 1*time.Second, "echo", "hello merlin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello merlin" {
		t.Fatalf("expected 'hello merlin', got %q", string(out))
	}
}

func TestRunCommandWithTimeout_Timeout(t *testing.T) {
	ctx := context.Background()
	// sleep 2 秒但超时限制为 100 毫秒
	_, err := RunCommandWithTimeout(ctx, 100*time.Millisecond, "sleep", "2")
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
}
```

- [ ] **Step 3: 运行测试验证失败**

```bash
go test ./pkg/util/...
```
Expected: FAIL 提示 `RunCommandWithTimeout` 未定义。

- [ ] **Step 4: 实现 `pkg/util/exec.go`**

```go
package util

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// RunCommandWithTimeout 在指定超时内执行系统命令，防止底层驱动或 CLI 挂起
func RunCommandWithTimeout(parentCtx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("command %s %v timed out after %v", name, args, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("command %s %v failed: %w, stderr: %s", name, args, err, stderr.String())
	}

	return stdout.Bytes(), nil
}
```

- [ ] **Step 5: 编写 `pkg/nvram/nvram_test.go`**

```go
package nvram

import (
	"context"
	"testing"
)

func TestMockNVRAMClient(t *testing.T) {
	mockData := map[string]string{
		"productid": "RT-AX86U",
		"buildno":   "3004.388.7",
	}
	client := NewMockClient(mockData)

	val, err := client.Get(context.Background(), "productid")
	if err != nil || val != "RT-AX86U" {
		t.Fatalf("expected RT-AX86U, got %s (err: %v)", val, err)
	}

	all, err := client.GetAll(context.Background(), []string{"productid", "buildno", "unknown"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if all["productid"] != "RT-AX86U" || all["buildno"] != "3004.388.7" || all["unknown"] != "" {
		t.Fatalf("unexpected map: %v", all)
	}
}
```

- [ ] **Step 6: 实现 `pkg/nvram/nvram.go`**

```go
package nvram

import (
	"context"
	"strings"
	"time"

	"metrics/pkg/util"
)

type Client interface {
	Get(ctx context.Context, key string) (string, error)
	GetAll(ctx context.Context, keys []string) (map[string]string, error)
}

type realClient struct {
	timeout time.Duration
}

func NewClient(timeout time.Duration) Client {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &realClient{timeout: timeout}
}

func (c *realClient) Get(ctx context.Context, key string) (string, error) {
	out, err := util.RunCommandWithTimeout(ctx, c.timeout, "nvram", "get", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *realClient) GetAll(ctx context.Context, keys []string) (map[string]string, error) {
	res := make(map[string]string, len(keys))
	for _, k := range keys {
		val, err := c.Get(ctx, k)
		if err == nil {
			res[k] = val
		} else {
			res[k] = ""
		}
	}
	return res, nil
}

type mockClient struct {
	data map[string]string
}

func NewMockClient(data map[string]string) Client {
	return &mockClient{data: data}
}

func (m *mockClient) Get(ctx context.Context, key string) (string, error) {
	return m.data[key], nil
}

func (m *mockClient) GetAll(ctx context.Context, keys []string) (map[string]string, error) {
	res := make(map[string]string, len(keys))
	for _, k := range keys {
		res[k] = m.data[k]
	}
	return res, nil
}
```

- [ ] **Step 7: 执行测试确认全部通过**

```bash
go test ./pkg/... -v
```
Expected: PASS

- [ ] **Step 8: 提交代码**

```bash
git add go.mod go.sum pkg/
git commit -m "feat(pkg): add command execution with timeout and nvram client"
```

---

### Task 2: 核心 Collector 接口、注册中心与聚合调度器 (`collector/collector.go`)

**Files:**
- Create: `collector/collector.go`
- Create: `collector/collector_test.go`

**Interfaces:**
- Produces:
  - `collector.Collector`: 统一接口，包含 `Name() string` 与 `Update(ctx context.Context, ch chan<- prometheus.Metric) error`
  - `collector.RegisterCollector(name string, defaultState bool, factory Factory)`
  - `collector.NewMerlinCollector(timeout time.Duration, enabled map[string]bool) (*MerlinCollector, error)`

- [ ] **Step 1: 编写 `collector_test.go`**

```go
package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type dummyCollector struct{}

func (d *dummyCollector) Name() string { return "dummy" }
func (d *dummyCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	desc := prometheus.NewDesc("router_dummy_metric", "Dummy metric", nil, nil)
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 42)
	return nil
}

func TestMerlinCollector_Collect(t *testing.T) {
	RegisterCollector("dummy", true, func() (Collector, error) {
		return &dummyCollector{}, nil
	})

	mc, err := NewMerlinCollector(1*time.Second, map[string]bool{"dummy": true})
	if err != nil {
		t.Fatalf("failed to create MerlinCollector: %v", err)
	}

	ch := make(chan prometheus.Metric, 10)
	mc.Collect(ch)
	close(ch)

	foundDummy := false
	for m := range ch {
		if m.Desc().String() != "" {
			foundDummy = true
		}
	}
	if !foundDummy {
		t.Fatalf("expected metric from dummy collector")
	}
}
```

- [ ] **Step 2: 运行测试验证失败**

```bash
go test ./collector -run TestMerlinCollector_Collect
```
Expected: FAIL 提示未定义的类型。

- [ ] **Step 3: 实现 `collector/collector.go`**

```go
package collector

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const Namespace = "router"

var (
	scrapeDurationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "exporter", "scrape_duration_seconds"),
		"Scrape duration of each collector in seconds.",
		[]string{"collector"}, nil,
	)
	scrapeSuccessDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "exporter", "collector_success"),
		"Whether a collector scrape succeeded (1) or failed (0).",
		[]string{"collector"}, nil,
	)
	lastScrapeTimestampDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "exporter", "last_scrape_timestamp_seconds"),
		"Unix timestamp in seconds when the exporter last scraped metrics.",
		nil, nil,
	)
)

type Collector interface {
	Name() string
	Update(ctx context.Context, ch chan<- prometheus.Metric) error
}

type Factory func() (Collector, error)

var (
	factories       = make(map[string]Factory)
	defaultStatuses = make(map[string]bool)
	factoriesMu     sync.RWMutex
)

func RegisterCollector(name string, defaultState bool, factory Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[name] = factory
	defaultStatuses[name] = defaultState
}

func AvailableCollectors() map[string]bool {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	res := make(map[string]bool, len(defaultStatuses))
	for k, v := range defaultStatuses {
		res[k] = v
	}
	return res
}

type MerlinCollector struct {
	collectors    map[string]Collector
	scrapeTimeout time.Duration
}

func NewMerlinCollector(timeout time.Duration, enabled map[string]bool) (*MerlinCollector, error) {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()

	active := make(map[string]Collector)
	for name, factory := range factories {
		isEnabled := defaultStatuses[name]
		if override, ok := enabled[name]; ok {
			isEnabled = override
		}
		if isEnabled {
			col, err := factory()
			if err != nil {
				return nil, fmt.Errorf("failed to init collector %s: %w", name, err)
			}
			active[name] = col
		}
	}

	return &MerlinCollector{
		collectors:    active,
		scrapeTimeout: timeout,
	}, nil
}

func (m *MerlinCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- scrapeDurationDesc
	ch <- scrapeSuccessDesc
	ch <- lastScrapeTimestampDesc
}

func (m *MerlinCollector) Collect(ch chan<- prometheus.Metric) {
	now := float64(time.Now().Unix())
	ch <- prometheus.MustNewConstMetric(lastScrapeTimestampDesc, prometheus.GaugeValue, now)

	ctx, cancel := context.WithTimeout(context.Background(), m.scrapeTimeout)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(len(m.collectors))

	for name, col := range m.collectors {
		go func(n string, c Collector) {
			defer wg.Done()
			start := time.Now()
			err := c.Update(ctx, ch)
			duration := time.Since(start).Seconds()

			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, duration, n)
			success := 1.0
			if err != nil {
				success = 0.0
				log.Printf("[ERROR] Collector %s failed: %v", n, err)
			}
			ch <- prometheus.MustNewConstMetric(scrapeSuccessDesc, prometheus.GaugeValue, success, n)
		}(name, col)
	}

	wg.Wait()
}
```

- [ ] **Step 4: 执行测试确认通过**

```bash
go test ./collector -v
```
Expected: PASS

- [ ] **Step 5: 提交代码**

```bash
git add collector/
git commit -m "feat(collector): implement core collector interface and merlin aggregator"
```

---

### Task 3: 系统资源采集器 (`collector/system.go`)

**Files:**
- Create: `collector/system.go`
- Create: `collector/system_test.go`

**Interfaces:**
- Produces:
  - `systemCollector` 注册至 `system`
  - 暴露指标：`router_load1`, `router_load5`, `router_load15`, `router_memory_*_bytes`, `router_uptime_seconds`

- [ ] **Step 1: 编写 `collector/system_test.go`**

```go
package collector

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestParseLoadavg(t *testing.T) {
	content := "0.45 0.32 0.28 1/142 12345\n"
	l1, l5, l15, err := parseLoadavg(strings.NewReader(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l1 != 0.45 || l5 != 0.32 || l15 != 0.28 {
		t.Fatalf("unexpected values: %v, %v, %v", l1, l5, l15)
	}
}

func TestParseMeminfo(t *testing.T) {
	content := `MemTotal:         508824 kB
MemFree:          123456 kB
MemAvailable:     234567 kB
Buffers:           12000 kB
Cached:            98765 kB
`
	mem, err := parseMeminfo(strings.NewReader(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mem["MemTotal"] != 508824*1024 || mem["MemFree"] != 123456*1024 {
		t.Fatalf("unexpected memory conversion: %v", mem)
	}
}
```

- [ ] **Step 2: 运行测试验证失败**

```bash
go test ./collector -run TestParseLoadavg
```
Expected: FAIL 提示函数未定义。

- [ ] **Step 3: 实现 `collector/system.go`**

包含 `/proc/loadavg`、`/proc/meminfo` 和 `/proc/uptime` 的健壮流式解析与指标构建。

- [ ] **Step 4: 运行全部测试**

```bash
go test ./collector -v
```
Expected: PASS

- [ ] **Step 5: 提交代码**

```bash
git add collector/system.go collector/system_test.go
git commit -m "feat(collector): implement system resource collector"
```

---

### Task 4: 网络接口流量采集器 (`collector/netdev.go`)

**Files:**
- Create: `collector/netdev.go`
- Create: `collector/netdev_test.go`

**Interfaces:**
- Produces:
  - `netdevCollector` 注册至 `netdev`
  - 暴露指标：`router_network_receive_bytes_total`, `router_network_transmit_bytes_total`, `router_network_receive_packets_total`, `router_network_transmit_packets_total`, `router_network_receive_errors_total`, `router_network_transmit_errors_total`

- [ ] **Step 1: 编写 `collector/netdev_test.go` 解析 `/proc/net/dev` 测试**
- [ ] **Step 2: 运行测试验证失败**
- [ ] **Step 3: 实现 `collector/netdev.go`（支持排除 `lo` 与自定义正则）**
- [ ] **Step 4: 运行测试确保全部通过**
- [ ] **Step 5: 提交代码**

```bash
git add collector/netdev.go collector/netdev_test.go
git commit -m "feat(collector): implement netdev traffic collector"
```

---

### Task 5: 硬件与 Wi-Fi 温度采集器 (`collector/thermal.go`)

**Files:**
- Create: `collector/thermal.go`
- Create: `collector/thermal_test.go`

**Interfaces:**
- Produces:
  - `thermalCollector` 注册至 `thermal`
  - 暴露指标：`router_cpu_temperature_celsius`, `router_wifi_temperature_celsius`

- [ ] **Step 1: 编写 `wl phy_tempsense` 与 CPU sysfs 的解析单元测试**
- [ ] **Step 2: 运行测试验证失败**
- [ ] **Step 3: 实现 `collector/thermal.go`，集成 context 超时与回退逻辑**
- [ ] **Step 4: 运行测试验证**
- [ ] **Step 5: 提交代码**

```bash
git add collector/thermal.go collector/thermal_test.go
git commit -m "feat(collector): implement thermal and wifi temperature collector"
```

---

### Task 6: 无线频段与连接客户端采集器 (`collector/wireless.go`)

**Files:**
- Create: `collector/wireless.go`
- Create: `collector/wireless_test.go`

**Interfaces:**
- Produces:
  - `wirelessCollector` 注册至 `wireless`
  - 暴露指标：`router_wifi_connected_clients`, `router_wifi_radio_enabled`

- [ ] **Step 1: 编写 NVRAM 网卡探测与 `wl assoclist` 输出 MAC 解析测试**
- [ ] **Step 2: 运行测试验证失败**
- [ ] **Step 3: 实现 `collector/wireless.go`（动态探测 `wl0_ifname`, `wl1_ifname`, `wl2_ifname`）**
- [ ] **Step 4: 运行测试确认通过**
- [ ] **Step 5: 提交代码**

```bash
git add collector/wireless.go collector/wireless_test.go
git commit -m "feat(collector): implement wireless clients and radio status collector"
```

---

### Task 7: 路由器专有状态与网关元信息采集器 (`collector/nvram.go`)

**Files:**
- Create: `collector/nvram.go`
- Create: `collector/nvram_collector_test.go`

**Interfaces:**
- Produces:
  - `nvramCollector` 注册至 `nvram`
  - 暴露指标：`router_info`, `router_network_mode`, `router_gateway_info`, `router_wan_status`, `router_wan_internet_status`, `router_wan_uptime_seconds`, `router_ntp_synced`

- [ ] **Step 1: 编写 NVRAM 数据映射与状态计算的单元测试**
- [ ] **Step 2: 运行测试验证失败**
- [ ] **Step 3: 实现 `collector/nvram.go`**
- [ ] **Step 4: 运行测试确认通过**
- [ ] **Step 5: 提交代码**

```bash
git add collector/nvram.go collector/nvram_collector_test.go
git commit -m "feat(collector): implement router nvram gateway and wan status collector"
```

---

### Task 8: 局域网终端与心跳采集器 (`collector/lan_client.go`)

**Files:**
- Create: `collector/lan_client.go`
- Create: `collector/lan_client_test.go`

**Interfaces:**
- Produces:
  - `lanClientCollector` 注册至 `lan_client`
  - 暴露指标：`router_lan_client_info`, `router_lan_client_lease_expires_timestamp_seconds`, `router_lan_client_last_heartbeat_timestamp_seconds`, `router_lan_client_active`, `router_lan_client_receive_bytes_total`, `router_lan_client_transmit_bytes_total`

- [ ] **Step 1: 编写 `dnsmasq.leases` 与 `/proc/net/arp` 解析与心跳推算测试**
- [ ] **Step 2: 运行测试验证失败**
- [ ] **Step 3: 实现 `collector/lan_client.go`，融合 DHCP 与静态设备及流量统计**
- [ ] **Step 4: 运行测试确认通过**
- [ ] **Step 5: 提交代码**

```bash
git add collector/lan_client.go collector/lan_client_test.go
git commit -m "feat(collector): implement fine-grained lan clients and heartbeat collector"
```

---

### Task 9: 程序主入口与命令行参数动态映射 (`cmd/merlin_exporter/main.go`)

**Files:**
- Create: `cmd/merlin_exporter/main.go`
- Remove: 根目录旧模板 `main.go`

**Interfaces:**
- CLI Flags:
  - `--web.listen-address` (默认 `:9101`)
  - `--web.telemetry-path` (默认 `/metrics`)
  - `--scrape.timeout` (默认 `5s`)
  - `--collector.<name>` (动态绑定已注册的所有 Collector)
  - `--version`

- [ ] **Step 1: 编写 `cmd/merlin_exporter/main.go`**
- [ ] **Step 2: 清理根目录旧的 `main.go` 模板**
- [ ] **Step 3: 本地编译并运行测试帮助信息与指标暴露**

```bash
go run cmd/merlin_exporter/main.go --help
```
Expected: 显示所有已注册的 `--collector.xxx` 参数及配置选项。

- [ ] **Step 4: 本地启动服务并发起 curl 请求验证**

```bash
curl -s http://localhost:9101/metrics | grep router_
```
Expected: 返回标准 Prometheus 指标格式文本。

- [ ] **Step 5: 提交代码**

```bash
git add cmd/merlin_exporter/main.go
git rm main.go
git commit -m "feat: complete application entrypoint and dynamic flag bindings"
```

---

### Task 10: 自动化构建脚本、双架构静态交叉编译与 UPX 压缩 (`Makefile`)

**Files:**
- Create: `Makefile`

**Targets:**
- `build`: 本地构建
- `release-arm64`: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` + strip + UPX
- `release-armv7`: `GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0` + strip + UPX
- `release`: 打包 tar.gz 发行包（包含二进制、自启脚本与 README）

- [ ] **Step 1: 编写完整的 `Makefile`**
- [ ] **Step 2: 验证本地构建与交叉编译构建**

```bash
make release-arm64
make release-armv7
```
Expected: 在 `bin/` 目录下生成静态二进制文件并输出体积。

- [ ] **Step 3: 提交代码**

```bash
git add Makefile
git commit -m "build: add makefile for cross-compilation and upx compression"
```

---

### Task 11: 开机自启动脚本模板与 Grafana 监控大盘 (`scripts/` 与 `dashboards/`)

**Files:**
- Create: `scripts/services-start.sample`
- Create: `dashboards/merlin_overview.json`
- Create: `README.md`

- [ ] **Step 1: 编写 `scripts/services-start.sample` 脚本**
- [ ] **Step 2: 编写完整的 Grafana Dashboard JSON 模板 `dashboards/merlin_overview.json`**
- [ ] **Step 3: 编写项目说明文档 `README.md`（包含架构、快速开始、部署指导与指标字典）**
- [ ] **Step 4: 提交代码**

```bash
git add scripts/ dashboards/ README.md
git commit -m "docs: add deployment script, grafana dashboard, and user guide"
```
