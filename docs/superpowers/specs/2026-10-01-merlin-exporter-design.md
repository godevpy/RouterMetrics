# 技术设计规范：merlin_exporter

**日期：** 2026-10-01  
**目标平台：** Asuswrt-Merlin 固件路由器（Broadcom ARM64 / ARMv7 芯片方案）  
**部署路径：** `/jffs/bin/merlin_exporter`  
**自启动管理：** `/jffs/scripts/services-start`  

---

## 1. 项目背景与硬件/系统约束

`merlin_exporter` 是一款专为运行 **Asuswrt-Merlin** 固件的家用与企业级路由器（主要基于博通 Broadcom 芯片方案，如 RT-AX86U、GT-AX6000、RT-AC86U 及 Wi-Fi 7 BE 系列）深度定制的超轻量级 Prometheus 指标采集端。

### 核心环境约束与技术特性：
1. **处理器架构**：Linux `arm64` (aarch64) 与 `armv7l` (armhf)。
2. **存储空间极致受限**：部署于 `/jffs/bin/` 分区（可用空间仅数十 MB），剥离调试符号与 UPX 压缩后的目标二进制必须严格控制在 **2.5MB ~ 3.5MB** 以内。
3. **常驻内存要求严苛**：空闲与抓取阶段的常驻物理内存需控制在 **5MB ~ 8MB** 之间，防止内存超载引发 OOM。因此排除了官方较为臃肿的 `node_exporter`。
4. **纯静态交叉编译**：必须支持无 CGO 依赖编译（`CGO_ENABLED=0`），剔除符号表（`-ldflags="-s -w"`）及 `upx` 压缩。
5. **初始化系统**：无 systemd 或 sysvinit，完全依赖 Merlin 固件原生钩子脚本 `/jffs/scripts/services-start` 启动后台守护进程。
6. **多维数据源融合**：
   - Linux 内核文件系统：`/proc/loadavg`、`/proc/meminfo`、`/proc/net/dev`、`/proc/uptime`、`/proc/net/arp`。
   - Broadcom 芯片温度 sysfs：`/sys/class/thermal/thermal_zone*/temp`。
   - Broadcom 无线驱动 CLI：`wl -i <iface> phy_tempsense`、`wl -i <iface> assoclist`、`wl -i <iface> sta_info <mac>`。
   - 固件配置数据库：`nvram get <key>`。
   - DHCP 租约文件：`/var/lib/misc/dnsmasq.leases`。

---

## 2. 整体架构与项目目录结构

采用 Go 官方推荐标准目录布局，杜绝重型外部 CLI 框架，保持依赖纯净：

```
metrics/
├── cmd/
│   └── merlin_exporter/
│       └── main.go              # 程序入口：CLI 参数解析、HTTP 路由、promhttp 注册
├── collector/
│   ├── collector.go             # 核心抽象：Collector 接口、Registry 注册表、MerlinCollector 聚合实现
│   ├── system.go                # 系统指标：/proc/loadavg, /proc/meminfo, /proc/uptime
│   ├── netdev.go                # 网络接口：/proc/net/dev（支持正则过滤虚拟/回环网卡）
│   ├── thermal.go               # 硬件温度：CPU thermal zones 与 wl phy_tempsense
│   ├── wireless.go              # 无线状态：NVRAM 网卡动态探测与 wl assoclist 连接数
│   ├── nvram.go                 # 路由器专有信息：机型、固件版本、WAN 状态/IP/Uptime、网络模式、NTP
│   └── lan_client.go            # 局域网终端：dnsmasq 租约 + ARP 表 + 心跳时间 + 终端流量
├── pkg/
│   ├── nvram/                   # NVRAM 读取封装（注入 Context 超时与批量读取优化）
│   │   └── nvram.go
│   └── util/                    # 底层执行工具（安全超时执行 exec.Command、数据格式解析）
│       └── exec.go
├── scripts/
│   └── services-start.sample    # Asuswrt-Merlin 开机自启脚本模板
├── dashboards/
│   └── merlin_overview.json     # 配套预制 Grafana 监控大盘
├── Makefile                     # 自动化构建：交叉编译 (arm64, armv7) + UPX 压缩 + 发布打包
├── go.mod
└── go.sum
```

---

## 3. 核心抽象与并发执行模型

### 3.1 统一 Collector 接口
各子系统必须实现统一的 `Collector` 接口：

```go
package collector

import (
    "context"
    "github.com/prometheus/client_golang/prometheus"
)

type Collector interface {
    Name() string
    Update(ctx context.Context, ch chan<- prometheus.Metric) error
}

type Factory func() (Collector, error)
```

### 3.2 注册中心与动态 Flag 绑定
- 各 Collector 模块通过 `init()` 自动向全局注册中心登记：`RegisterCollector(name string, defaultState bool, factory Factory)`。
- 程序启动时，基于 Go 原生标准库 `flag` 动态映射各模块开关（如 `--collector.thermal=true/false`），零大型框架依赖。

### 3.3 并发抓取、超时控制与故障隔离
当 Prometheus 触发 HTTP GET `/metrics` 抓取时：
1. 聚合器 `MerlinCollector` 创建带有全局超时的上下文（默认 5s）：
   ```go
   ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
   defer cancel()
   ```
2. 利用 `sync.WaitGroup` 并发调度所有启用的 Collector。
3. **错误隔离**：单个模块失败（如无线驱动调用 `wl` 超时或特定硬件节点缺失），仅记录日志并标记自身成功状态为 0，绝对不阻塞或中断其他 Collector 的正常上报。
4. **自监控指标暴露**：
   - `router_exporter_scrape_duration_seconds{collector="<name>"}`：各模块耗时。
   - `router_exporter_collector_success{collector="<name>"}`：采集成功（1）或失败（0）。
   - `router_exporter_last_scrape_timestamp_seconds`：本次抓取的 Unix 秒级时间戳心跳。

---

## 4. 各模块采集器与指标定义规范

### 4.1 系统资源采集器（`collector/system.go`）
- **数据源**：`/proc/loadavg`、`/proc/meminfo`、`/proc/uptime`
- **暴露指标**：
  - `router_load1`, `router_load5`, `router_load15`（Gauge，1/5/15 分钟系统平均负载）
  - `router_memory_total_bytes`（Gauge，总物理内存）
  - `router_memory_free_bytes`（Gauge，空闲物理内存）
  - `router_memory_available_bytes`（Gauge，可用物理内存）
  - `router_memory_buffers_bytes`（Gauge，缓冲区占用）
  - `router_memory_cached_bytes`（Gauge，缓存占用）
  - `router_uptime_seconds`（Counter，系统累计运行秒数）

### 4.2 网络接口流量采集器（`collector/netdev.go`）
- **数据源**：`/proc/net/dev`
- **过滤机制**：默认排除回环网卡 `lo` 及虚拟接口，支持通过参数自定义 include/exclude 正则。
- **暴露指标**：
  - `router_network_receive_bytes_total{device="..."}`（Counter，接口接收字节总数）
  - `router_network_transmit_bytes_total{device="..."}`（Counter，接口发送字节总数）
  - `router_network_receive_packets_total{device="..."}`（Counter，接口接收数据包总数）
  - `router_network_transmit_packets_total{device="..."}`（Counter，接口发送数据包总数）
  - `router_network_receive_errors_total{device="..."}`（Counter，接口接收错误数）
  - `router_network_transmit_errors_total{device="..."}`（Counter，接口发送错误数）

### 4.3 硬件温度采集器（`collector/thermal.go`）
- **CPU 温度**：
  - 自动扫描 `/sys/class/thermal/thermal_zone*/temp`，读取毫摄氏度除以 1000 转换为标准摄氏度。
- **无线芯片温度**：
  - 对探测到的无线接口执行 `wl -i <iface> phy_tempsense`（带超时 800ms）。
  - 解析输出的首个数字（如 `55 (0x37)` -> 55.0°C）。
- **暴露指标**：
  - `router_cpu_temperature_celsius{zone="..."}`（Gauge，CPU 核心温度）
  - `router_wifi_temperature_celsius{interface="...", band="..."}`（Gauge，无线芯片温度）

### 4.4 无线射频与连接采集器（`collector/wireless.go`）
- **接口动态自适应探测**：
  - 查询 NVRAM 对应频段网卡变量：
    - 2.4GHz: `wl0_ifname`（例如 `eth6` 或 `wl0`）
    - 5GHz-1: `wl1_ifname`（例如 `eth7` 或 `wl1`）
    - 5GHz-2 / 6GHz: `wl2_ifname`（例如 `eth8` 或 `wl2`）
  - 获取射频开关状态 `wl0_radio` 及广播 SSID。
- **连接客户端统计**：
  - 执行 `wl -i <iface> assoclist`，统计关联 MAC 地址行数。
- **暴露指标**：
  - `router_wifi_connected_clients{interface="...", band="...", ssid="..."}`（Gauge，各频段连接设备数）
  - `router_wifi_radio_enabled{interface="...", band="..."}`（Gauge，射频启用状态，1 为开启）

### 4.5 路由器专有状态与网关元数据采集器（`collector/nvram.go`）
- **设备信息与工作模式**：
  - 读取 `productid`、`buildno`、`extendno`、`sw_mode`（1 为无线路由网关模式，2 为中继模式，3 为 AP 模式）。
- **WAN 外网与网关连通性**：
  - 读取 `wan0_state_t`（2 为连接成功）、`wan0_ipaddr`、`wan0_gateway`、`wan0_proto`、`wan0_dns`、`wan0_uptime`。
  - 读取 Merlin 固件原生外网探测状态 `link_internet`（2 为 Internet 正常连通）以及 NTP 同步状态 `ntp_ready`。
- **暴露指标**：
  - `router_info{product_id="...", firmware_version="...", architecture="..."}`（Gauge，恒为 1，携带基础元信息）
  - `router_network_mode{mode="..."}`（Gauge，恒为 1，标识路由模式）
  - `router_gateway_info{lan_ip="...", lan_netmask="...", wan_ip="...", wan_gateway="...", dns="..."}`（Gauge，恒为 1）
  - `router_wan_status{interface="wan0", ip="...", proto="..."}`（Gauge，1 为正常连通，0 为断线）
  - `router_wan_internet_status`（Gauge，1 为外网正常连通，0 为无网络访问）
  - `router_wan_uptime_seconds`（Gauge，WAN 口已连接秒数）
  - `router_ntp_synced`（Gauge，1 为时钟已同步，0 为未同步）

### 4.6 局域网终端与心跳采集器（`collector/lan_client.go`）
- **多源数据融合**：
  - 读取 `/var/lib/misc/dnsmasq.leases`：获取每个 DHCP 分配设备的到期时间戳、MAC、IP、主机名。
  - 读取 `/proc/net/arp`：提取有效活跃的静态 IP 设备（`Flags != 0x0` 且不在 DHCP 列表中），标记 `is_dhcp="false"`。
  - **心跳推算**：
    结合 NVRAM 中的租约时长 `dhcp_lease`（如 86400s），精确推算上次 DHCP 续租心跳时间：
    $$\text{last\_heartbeat} = \text{expires\_timestamp} - \text{lease\_duration}$$
- **终端流量采集**：
  - 针对无线客户端，通过 `wl -i <iface> sta_info <mac>` 获取终端下行 `tx_bytes` 与上行 `rx_bytes`。
  - 包含命令行开关控制 `--collector.lan-client.traffic=true`。
- **暴露指标**：
  - `router_lan_client_info{ip="...", mac="...", hostname="...", is_dhcp="true/false", type="wifi/wired"}`（Gauge，恒为 1）
  - `router_lan_client_lease_expires_timestamp_seconds{ip="...", mac="..."}`（Gauge，租约到期时间戳）
  - `router_lan_client_last_heartbeat_timestamp_seconds{ip="...", mac="..."}`（Gauge，上次心跳时间戳）
  - `router_lan_client_active{ip="...", mac="..."}`（Gauge，ARP 活跃状态：1 或 0）
  - `router_lan_client_receive_bytes_total{ip="...", mac="..."}`（Counter，终端接收字节总数）
  - `router_lan_client_transmit_bytes_total{ip="...", mac="..."}`（Counter，终端发送字节总数）

---

## 5. 构建、打包与生产部署规范

### 5.1 自动化构建流水线（`Makefile`）
- 编译参数设置：
  ```makefile
  GO_BUILD_FLAGS = -ldflags="-s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)"
  CGO_ENABLED = 0
  ```
- 交叉编译目标：
  - `linux/arm64`（适用于 RT-AX86U, GT-AX6000 等主流机型）
  - `linux/armv7`（适用于 RT-AC86U, RT-AX56U 等 32 位机型）
- UPX 深度压缩：使用 `upx --best --lzma`，将最终二进制文件压缩至 2.5MB ~ 3.5MB。

### 5.2 开机自启动守护进程（`scripts/services-start.sample`）
- 部署到 `/jffs/scripts/services-start` 并赋予执行权限（`chmod +x`）。
- 具备防重叠运行检测与自动后台托管。

### 5.3 监控可视化看板（`dashboards/merlin_overview.json`）
- **路由器概览状态栏**：机型、固件版本、LAN 网关 IP、WAN IP、WAN 连接状态、在线设备总数、Uptime。
- **系统核心负载与内存**：CPU 负载曲线（1m/5m/15m）、内存使用分布图（已用、空闲、缓存）。
- **硬件温度监控**：CPU 温度、各频段无线芯片温度，配置 75°C 警戒黄线与 85°C 危险红线。
- **网络接口带宽监控**：WAN 口实时吞吐速率（Mbps）、LAN 核心网口流量与数据包错误统计。
- **无线与内网设备监控**：各 Wi-Fi 频段连接数曲线，内网活跃客户端全量清单表格（IP、MAC、主机名、是否 DHCP、心跳时间、流量）。
- **Exporter 自身监控**：各采集器抓取耗时（ms）与成功率。

---

## 6. 测试与质量保证策略

1. **单元测试与虚拟解析**：
   - 针对 `/proc/loadavg`、`/proc/meminfo`、`/proc/net/dev`、`/proc/net/arp` 以及 `dnsmasq.leases` 编写全套测试用例，提供仿真测试样本。
   - 编写 `wl phy_tempsense`、`wl assoclist` 输出文本解析的单元测试。
2. **外部依赖 Mock 隔离**：
   - 在 `pkg/util/exec.go` 中提供命令执行的 Mock 接口，在无路由器真机环境（如 macOS / x86 CI）下依然能完整验证超时打断与错误隔离逻辑。
3. **交叉编译交付验证**：
   - 验证 `make release-arm64` 与 `make release-armv7` 在本地能否稳定生成无 CGO 依赖的静态二进制文件。
