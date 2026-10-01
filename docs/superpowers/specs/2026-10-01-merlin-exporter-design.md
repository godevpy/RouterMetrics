# Technical Design Specification: merlin_exporter

**Date:** 2026-10-01  
**Target Platform:** Asuswrt-Merlin (Broadcom ARM64 / ARMv7)  
**Binary Location:** `/jffs/bin/merlin_exporter`  
**Daemon Management:** `/jffs/scripts/services-start`  

---

## 1. Project Background and Environmental Constraints

`merlin_exporter` is a lightweight, custom Prometheus exporter tailored specifically for home and enterprise routers powered by the **Asuswrt-Merlin** firmware (primarily Broadcom chipsets such as RT-AX86U, GT-AX6000, RT-AC86U, and Wi-Fi 7 BE-series).

### System & Hardware Constraints
1. **Architecture**: Linux `arm64` (aarch64) and `armv7l` (armhf).
2. **Flash Storage**: Deployed onto `/jffs/bin/` where space is typically tens of megabytes. Target executable size after strip and UPX compression must be within **2.5 ~ 3.5 MB**.
3. **RAM Footprint**: Resident memory must stay strictly within **5 ~ 8 MB** during idle and scrape phases, preventing OOM crashes on memory-constrained routers. Heavy official exporters such as `node_exporter` are not suitable.
4. **Build Architecture**: Pure static build without CGO (`CGO_ENABLED=0`), stripped symbols (`-ldflags="-s -w"`), and UPX compression.
5. **Init System**: No systemd or sysvinit; relies on Asuswrt-Merlin user script hooks (`/jffs/scripts/services-start`).
6. **Data Sources**:
   - Kernel filesystems: `/proc/loadavg`, `/proc/meminfo`, `/proc/net/dev`, `/proc/uptime`, `/proc/net/arp`.
   - Broadcom sysfs: `/sys/class/thermal/thermal_zone*/temp`.
   - Broadcom Wireless CLI: `wl -i <iface> phy_tempsense`, `wl -i <iface> assoclist`, `wl -i <iface> sta_info <mac>`.
   - NVRAM CLI: `nvram get <key>`.
   - DNS/DHCP lease files: `/var/lib/misc/dnsmasq.leases`.

---

## 2. Architecture and Directory Layout

The codebase follows standard Go project conventions with zero heavy CLI/framework dependencies.

```
metrics/
├── cmd/
│   └── merlin_exporter/
│       └── main.go              # Entrypoint: flags parsing, HTTP server, promhttp integration
├── collector/
│   ├── collector.go             # Core interfaces: Collector, Registry, MerlinCollector
│   ├── system.go                # System: /proc/loadavg, /proc/meminfo, /proc/uptime
│   ├── netdev.go                # Network: /proc/net/dev (with device filtering)
│   ├── thermal.go               # Thermal: CPU thermal zones + wl phy_tempsense
│   ├── wireless.go              # Wi-Fi: nvram ifname discovery + wl assoclist clients
│   ├── nvram.go                 # NVRAM: product info, WAN status, IP, gateway, uptime, NTP
│   └── lan_client.go            # LAN Clients: dnsmasq.leases + /proc/net/arp + heartbeats + traffic
├── pkg/
│   ├── nvram/                   # Safe NVRAM reader with context timeout & cached bulk reads
│   │   └── nvram.go
│   └── util/                    # Command execution with timeout, parsing utilities
│       └── exec.go
├── scripts/
│   └── services-start.sample    # Asuswrt-Merlin auto-start daemon script
├── dashboards/
│   └── merlin_overview.json     # Pre-configured Grafana Dashboard
├── Makefile                     # Cross-compilation (arm64, armv7) + UPX + packaging
├── go.mod
└── go.sum
```

---

## 3. Core Collector Abstractions & Execution Model

### 3.1 Unified Interface
Each subsystem implements the modular `Collector` interface:

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

### 3.2 Registry and Flag Integration
- Collectors register their factory and default enabled state via `RegisterCollector(name string, defaultState bool, factory Factory)`.
- Command-line flags are generated dynamically using Go standard library `flag` (e.g., `--collector.<name>=true/false`). No heavy external CLI libraries (`kingpin`, `cobra`) are used.

### 3.3 Concurrency, Timeout, and Fault Isolation
When Prometheus scrapes `/metrics`:
1. `MerlinCollector.Collect(ch chan<- prometheus.Metric)` initiates a scoped context with scrape timeout (default `5s`):
   ```go
   ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
   defer cancel()
   ```
2. Each enabled collector runs concurrently in a worker goroutine using `sync.WaitGroup`.
3. If an individual collector fails or times out (e.g. Broadcom driver hangs on `wl phy_tempsense`), the error is logged and reported via `router_exporter_collector_success{collector="thermal"} = 0`, but other collectors continue uninterrupted.
4. Self-monitoring metrics exposed:
   - `router_exporter_scrape_duration_seconds{collector="<name>"}`
   - `router_exporter_collector_success{collector="<name>"}`
   - `router_exporter_last_scrape_timestamp_seconds`

---

## 4. Subsystem Collectors & Metrics Specification

### 4.1 System Collector (`collector/system.go`)
- **Sources**: `/proc/loadavg`, `/proc/meminfo`, `/proc/uptime`
- **Metrics**:
  - `router_load1`, `router_load5`, `router_load15` (Gauge)
  - `router_memory_total_bytes` (Gauge)
  - `router_memory_free_bytes` (Gauge)
  - `router_memory_available_bytes` (Gauge)
  - `router_memory_buffers_bytes` (Gauge)
  - `router_memory_cached_bytes` (Gauge)
  - `router_uptime_seconds` (Counter)

### 4.2 Network Device Collector (`collector/netdev.go`)
- **Sources**: `/proc/net/dev`
- **Filtering**: Ignores loopback `lo` and configurable virtual interfaces by default.
- **Metrics**:
  - `router_network_receive_bytes_total{device="..."}` (Counter)
  - `router_network_transmit_bytes_total{device="..."}` (Counter)
  - `router_network_receive_packets_total{device="..."}` (Counter)
  - `router_network_transmit_packets_total{device="..."}` (Counter)
  - `router_network_receive_errors_total{device="..."}` (Counter)
  - `router_network_transmit_errors_total{device="..."}` (Counter)

### 4.3 Thermal Collector (`collector/thermal.go`)
- **CPU Sources**:
  - Searches `/sys/class/thermal/thermal_zone*/temp` (millidegrees divided by 1000).
- **Wi-Fi Sources**:
  - For each wireless interface discovered, executes: `wl -i <iface> phy_tempsense` with context timeout (default 800ms).
  - Output parsed: e.g., `55 (0x37)` -> 55.0 °C.
- **Metrics**:
  - `router_cpu_temperature_celsius{zone="..."}` (Gauge)
  - `router_wifi_temperature_celsius{interface="...", band="..."}` (Gauge)

### 4.4 Wireless Collector (`collector/wireless.go`)
- **Interface Discovery**:
  - Queries NVRAM keys:
    - 2.4GHz: `wl0_ifname` (e.g. `eth6` or `wl0`)
    - 5GHz-1: `wl1_ifname` (e.g. `eth7` or `wl1`)
    - 5GHz-2 / 6GHz: `wl2_ifname` (e.g. `eth8` or `wl2`)
  - Also reads `wl0_ssid`, `wl1_ssid`, `wl0_radio`, `wl1_radio`.
- **Connected Clients**:
  - Executes `wl -i <iface> assoclist` with timeout.
  - Counts output MAC addresses.
- **Metrics**:
  - `router_wifi_connected_clients{interface="...", band="...", ssid="..."}` (Gauge)
  - `router_wifi_radio_enabled{interface="...", band="..."}` (Gauge)

### 4.5 NVRAM & Gateway Collector (`collector/nvram.go`)
- **Router Info & Mode**:
  - Reads `productid`, `buildno`, `extendno`, `sw_mode` (1 = Router, 2 = Repeater, 3 = AP).
  - Metrics:
    - `router_info{product_id="...", firmware_version="...", architecture="..."}` (Gauge = 1)
    - `router_network_mode{mode="..."}` (Gauge = 1)
- **WAN & Connectivity**:
  - Reads `wan0_state_t` (2 = connected), `wan0_ipaddr`, `wan0_gateway`, `wan0_proto`, `wan0_dns`, `wan0_uptime`, `link_internet` (2 = connected), `ntp_ready` (1 = synced).
  - Metrics:
    - `router_gateway_info{lan_ip="...", lan_netmask="...", wan_ip="...", wan_gateway="...", dns="..."}` (Gauge = 1)
    - `router_wan_status{interface="wan0", ip="...", proto="..."}` (Gauge: 1=connected, 0=disconnected)
    - `router_wan_internet_status` (Gauge: 1=connected, 0=disconnected)
    - `router_wan_uptime_seconds` (Gauge)
    - `router_ntp_synced` (Gauge: 1=synced, 0=not synced)

### 4.6 LAN Client Collector (`collector/lan_client.go`)
- **Data Source Fusion**:
  - Reads `/var/lib/misc/dnsmasq.leases`: extracts expiration timestamp, MAC, IP, hostname.
  - Reads `/proc/net/arp`: detects active static IP devices (Flags != 0x0) not present in DHCP leases.
  - Heartbeat calculation:
    $$\text{last\_heartbeat\_timestamp} = \text{expires\_timestamp} - \text{lease\_duration}$$
    (lease duration read from `nvram get dhcp_lease`, default 86400s).
- **Client Traffic (Wi-Fi & optional)**:
  - For Wi-Fi clients in `assoclist`, queries `wl -i <iface> sta_info <mac>` to obtain `tx_bytes` and `rx_bytes`.
- **Metrics**:
  - `router_lan_client_info{ip="...", mac="...", hostname="...", is_dhcp="true/false", type="wifi/wired"}` (Gauge = 1)
  - `router_lan_client_lease_expires_timestamp_seconds{ip="...", mac="..."}` (Gauge)
  - `router_lan_client_last_heartbeat_timestamp_seconds{ip="...", mac="..."}` (Gauge)
  - `router_lan_client_active{ip="...", mac="..."}` (Gauge: 1 or 0)
  - `router_lan_client_receive_bytes_total{ip="...", mac="..."}` (Counter, optional)
  - `router_lan_client_transmit_bytes_total{ip="...", mac="..."}` (Counter, optional)

---

## 5. Build, Packaging, and Deployment

### 5.1 Build Pipeline (`Makefile`)
- Compilation flags:
  ```makefile
  GO_BUILD_FLAGS = -ldflags="-s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)"
  CGO_ENABLED = 0
  ```
- Cross-compilation targets:
  - `linux/arm64` (GOOS=linux GOARCH=arm64)
  - `linux/armv7` (GOOS=linux GOARCH=arm GOARM=7)
- UPX compression:
  - `upx --best --lzma bin/merlin_exporter_arm64`
  - Targets 2.5MB ~ 3.5MB final size.

### 5.2 Auto-start Daemon (`scripts/services-start.sample`)
- Script deployed to `/jffs/scripts/services-start` with executable permissions (`chmod +x`).
- Ensures idempotence (kills previous instance before launching background process).

### 5.3 Grafana Dashboard (`dashboards/merlin_overview.json`)
- Visualizes:
  - Router metadata, WAN status, Gateway IP, Uptime.
  - CPU load, Memory breakdown, Thermal gauges and charts with alerts (>75°C warning, >85°C critical).
  - WAN and LAN throughput (Mbps) and packet counters.
  - Wi-Fi clients per band and LAN active client table (IP, MAC, Hostname, Heartbeat, Traffic).
  - Exporter scrape metrics (durations and status).

---

## 6. Testing and Verification Strategy

1. **Unit Tests**:
   - Parsers for `/proc/loadavg`, `/proc/meminfo`, `/proc/net/dev`, `/proc/net/arp`, and `dnsmasq.leases` using mock files.
   - Output parser for `wl phy_tempsense` and `wl assoclist`.
2. **Mocking External Commands**:
   - `pkg/util/exec.go` supports test injection to verify timeout handling and error isolation without running on real hardware.
3. **Cross-Compilation Verification**:
   - Validate that `make release-arm64` and `make release-armv7` produce statically linked, stripped, and UPX compressed binaries without CGO dependencies (`file` / `readelf` validation).
