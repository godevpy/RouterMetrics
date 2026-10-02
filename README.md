# merlin_exporter

专为运行 **Asuswrt-Merlin** 固件的路由器（如 RT-AX86U、GT-AX6000、RT-AC86U 及 Wi-Fi 7 BE 系列）深度定制的极轻量 Prometheus 指标采集端（Metrics Exporter）。

---

## 📌 项目特性与设计哲学

- **极致精简常驻内存**：无状态抓取设计，完全抛弃臃肿官方 `node_exporter`，常驻物理内存严格控制在 **5MB ~ 8MB** 以内。
- **纯静态与无 CGO 依赖**：`CGO_ENABLED=0` 静态编译，去除所有调试符号表（`-s -w`），原生适配 UPX 压缩至 **2.5MB ~ 3.5MB**，保护宝贵的 `/jffs` 分区闪存。
- **动态接口自适应识别**：启动或抓取时自动通过 NVRAM 解析无线接口名称（如 `eth6`/`eth7` 或 `wl0`/`wl1`），彻底告别硬编码。
- **细粒度内网终端与心跳监控**：
  - 融合 `dnsmasq.leases` 与 `/proc/net/arp`，同时监控 DHCP 与手动静态 IP 终端。
  - **精准推算内网设备上次 DHCP 心跳/续租时间戳**。
  - 区分终端 Wi-Fi / 有线连接方式，并支持抓取单终端收发流量。
- **硬核故障隔离与超时保护**：所有底层驱动命令（`wl`、`nvram`）均封装严格的 `context.WithTimeout`（800ms），单网卡或驱动卡死时平滑降级，绝不阻塞 Prometheus 抓取进程。
- **模块化插件式设计**：各子采集器通过标准库 `flag` 动态映射命令行参数（如 `--collector.thermal=false`），完全杜绝第三方大型 CLI 依赖。

---

## 📊 暴露核心指标一览

| 采集器 (Collector) | 指标名称 (Metric Name) | 类型 | 含义说明 |
| :--- | :--- | :--- | :--- |
| **system** | `router_load1`, `router_load5`, `router_load15` | Gauge | 系统 1/5/15 分钟平均负载 |
| | `router_memory_*_bytes` | Gauge | 总物理内存、空闲内存、可用内存、缓存与缓冲区 |
| | `router_uptime_seconds` | Counter | 路由器累计运行时间（秒） |
| **netdev** | `router_network_receive_bytes_total` | Counter | 网口累计下行接收字节数 |
| | `router_network_transmit_bytes_total` | Counter | 网口累计上行发送字节数 |
| | `router_network_receive_packets_total` | Counter | 网口接收数据包数（PPS） |
| | `router_network_transmit_packets_total` | Counter | 网口发送数据包数（PPS） |
| **thermal** | `router_cpu_temperature_celsius` | Gauge | CPU 核心温度（摄氏度） |
| | `router_wifi_temperature_celsius` | Gauge | 各频段无线芯片温度（摄氏度） |
| **wireless** | `router_wifi_connected_clients` | Gauge | 各 Wi-Fi 频段（2.4G/5G/6G）连接设备数 |
| | `router_wifi_radio_enabled` | Gauge | 各频段射频开启状态（1 开启，0 关闭） |
| **nvram** | `router_info` | Gauge | 机型、固件版本、架构等基础元信息 |
| | `router_network_mode` | Gauge | 运行模式（router / ap / repeater） |
| | `router_gateway_info` | Gauge | LAN 网关 IP、掩码、WAN IP、上级网关与 DNS |
| | `router_wan_status` | Gauge | WAN 口连接状态（1 连通，0 断开） |
| | `router_wan_internet_status` | Gauge | 外网互联网连通探测状态（1 正常，0 断网） |
| | `router_wan_uptime_seconds` | Gauge | WAN 连接保持时长（秒） |
| | `router_ntp_synced` | Gauge | NTP 时钟同步状态（1 已同步，0 未同步） |
| **lan_client** | `router_lan_client_info` | Gauge | 内网终端元数据（IP、MAC、主机名、是否DHCP、Wi-Fi/有线） |
| | `router_lan_client_active` | Gauge | 终端当前是否在 ARP 通信中处于活跃状态（1 在线，0 离线） |
| | `router_lan_client_last_heartbeat_timestamp_seconds`| Gauge | **终端上一次发起 DHCP 心跳续租的 Unix 秒级时间戳** |
| | `router_lan_client_lease_expires_timestamp_seconds` | Gauge | 终端 DHCP 租约到期时间戳 |
| | `router_lan_client_receive_bytes_total` | Counter | 终端下行接收字节总数（Wi-Fi 终端流量） |
| | `router_lan_client_transmit_bytes_total` | Counter | 终端上行发送字节总数（Wi-Fi 终端流量） |
| **自监控** | `router_exporter_scrape_duration_seconds` | Gauge | 各 Collector 本次采集耗时（秒） |
| | `router_exporter_collector_success` | Gauge | 各 Collector 本次抓取状态（1 成功，0 失败） |
| | `router_exporter_last_scrape_timestamp_seconds` | Gauge | Exporter 最新数据成功抓取时间戳心跳 |

---

## 🛠️ 构建指南 (Build Guide)

Makefile 中已预设多架构静态交叉编译配置：

```bash
# 1. 运行单元测试与竞态检测
make test

# 2. 交叉编译 Linux ARM64（推荐用于 RT-AX86U / GT-AX6000 / BE 等 64 位路由器）
make release-arm64

# 3. 交叉编译 Linux ARMv7（适用于 RT-AC86U / RT-AX56U 等 32 位路由器）
make release-armv7

# 4. 交叉编译 Linux AMD64（适用于 Ubuntu / x86 软路由测试）
make release-amd64

# 5. 一键生成全套发布包
make release
```

> **提示（UPX 压缩）**：在 Ubuntu 上安装 `upx-ucl`（`sudo apt install upx-ucl`）后执行 `make release`，构建系统会自动进行 `--best --lzma` 极限压缩，产物体积缩减至 3MB 左右。

---

## 🚀 Asuswrt-Merlin 路由器部署教程

### 1. 拷贝二进制到路由器
将编译好的 `merlin_exporter_arm64`（或 armv7）上传到路由器的 `/jffs/bin/` 目录：
```bash
# 在宿主机或 Ubuntu 上执行：
scp bin/merlin_exporter_arm64 admin@192.168.50.1:/jffs/bin/merlin_exporter

# 登录路由器并赋予可执行权限：
ssh admin@192.168.50.1
chmod +x /jffs/bin/merlin_exporter
```

### 2. 配置开机自启动守护进程
参考仓库提供的 [scripts/services-start.sample](file:///Users/itaca/code/asus/metrics/scripts/services-start.sample)，将其添加到路由器的 `/jffs/scripts/services-start`：

```bash
# 如果之前没有 services-start，直接新建：
cat << 'EOF' > /jffs/scripts/services-start
#!/bin/sh
BIN="/jffs/bin/merlin_exporter"
LOG="/tmp/merlin_exporter.log"

if [ -x "$BIN" ]; then
    killall -q $(basename "$BIN")
    sleep 1
    nohup "$BIN" --web.listen-address=":9101" > "$LOG" 2>&1 &
    logger -t "merlin_exporter" "Daemon started on port 9101"
fi
EOF

# 赋予执行权限
chmod +x /jffs/scripts/services-start

# 手动启动测试
/jffs/scripts/services-start
```

### 3. 验证端点连通
```bash
curl -s http://192.168.50.1:9101/metrics | grep router_
```

---

## 📈 Prometheus 与 Grafana 对接

### 1. Prometheus 配置 (`prometheus.yml`)
在您的 Prometheus 抓取配置中添加以下抓取任务：
```yaml
scrape_configs:
  - job_name: "merlin_router"
    scrape_interval: 15s
    scrape_timeout: 5s
    static_configs:
      - targets: ["192.168.50.1:9101"]
        labels:
          router: "home_gateway"
```

### 2. 导入 Grafana 大盘模板
1. 进入 Grafana 界面，点击 **Dashboards** -> **New** -> **Import**。
2. 上传本项目目录中的 [dashboards/merlin_overview.json](file:///Users/itaca/code/asus/metrics/dashboards/merlin_overview.json)。
3. 选择您配置好的 Prometheus 数据源，即可获得完整包含温度、流量、客户端心跳全景的现代化监控大盘！
