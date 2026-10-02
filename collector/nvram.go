// Package collector 实现各类系统的 Prometheus 指标采集器。
package collector

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"metrics/pkg/nvram"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// routerInfoDesc 描述路由器设备基础元信息（型号、固件版本、运行架构等）
	routerInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "", "info"),
		"Router basic metadata and firmware info.",
		[]string{"product_id", "firmware_version", "architecture"}, nil,
	)

	// routerNetworkModeDesc 描述路由器运行工作模式（路由、中继、AP、网桥等）
	routerNetworkModeDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "network", "mode"),
		"Router network operation mode (router, repeater, ap, media_bridge, unknown).",
		[]string{"mode"}, nil,
	)

	// routerGatewayInfoDesc 描述局域网网关及外网 WAN 网关的网络配置元数据
	routerGatewayInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "gateway", "info"),
		"Router LAN and WAN gateway network metadata.",
		[]string{"lan_ip", "lan_netmask", "wan_ip", "wan_gateway", "dns"}, nil,
	)

	// routerWanStatusDesc 描述 WAN 口物理与链路连通状态 (1 为已连通，0 为断开)
	routerWanStatusDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "wan", "status"),
		"WAN interface connection status (1 for connected, 0 for disconnected).",
		[]string{"interface", "ip", "proto"}, nil,
	)

	// routerWanInternetStatusDesc 描述路由器外网 Internet 探测连通状态 (1 为外网连通，0 为无外网访问)
	routerWanInternetStatusDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "wan", "internet_status"),
		"Internet connectivity probe status (1 for connected, 0 for disconnected).",
		nil, nil,
	)

	// routerWanUptimeDesc 描述 WAN 口自本次拨号/连通以来持续在线时长 (单位: 秒)
	routerWanUptimeDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "wan", "uptime_seconds"),
		"WAN connection uptime in seconds.",
		[]string{"interface"}, nil,
	)

	// routerNtpSyncedDesc 描述路由器 NTP 网络时间同步状态 (1 为已同步，0 为未同步)
	routerNtpSyncedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "", "ntp_synced"),
		"NTP time synchronization status (1 for synced, 0 for unsynced).",
		nil, nil,
	)
)

// nvramRequiredKeys 定义本次采集需一次性批量读取的所有 NVRAM 关键变量列表
var nvramRequiredKeys = []string{
	"productid",
	"buildno",
	"extendno",
	"sw_mode",
	"lan_ipaddr",
	"lan_netmask",
	"wan0_ipaddr",
	"wan0_gateway",
	"wan0_dns",
	"wan0_state_t",
	"wan0_proto",
	"wan0_uptime",
	"link_internet",
	"ntp_ready",
}

// nvramCollector 收集路由器专有状态与网关元信息指标
type nvramCollector struct {
	client       nvram.Client
	architecture string
}

// RegisterNVRAMCollector 将 NVRAM 状态采集器注册至采集器管理器
func RegisterNVRAMCollector() {
	RegisterCollector("nvram", true, NewNVRAMCollector)
}

func init() {
	RegisterNVRAMCollector()
}

// NewNVRAMCollector 创建使用默认 NVRAM 客户端配置的采集器
func NewNVRAMCollector() (Collector, error) {
	return NewNVRAMCollectorWithClient(nvram.NewClient(800 * time.Millisecond)), nil
}

// NewNVRAMCollectorWithClient 支持注入自定义 NVRAM 客户端的构造函数（用于单元测试与模拟环境）
func NewNVRAMCollectorWithClient(client nvram.Client) Collector {
	return NewNVRAMCollectorWithConfig(client, runtime.GOARCH)
}

// NewNVRAMCollectorWithConfig 支持完整依赖注入的构造函数（可指定 architecture 标签）
func NewNVRAMCollectorWithConfig(client nvram.Client, architecture string) Collector {
	if client == nil {
		client = nvram.NewClient(800 * time.Millisecond)
	}
	if architecture == "" {
		architecture = runtime.GOARCH
	}
	return &nvramCollector{
		client:       client,
		architecture: architecture,
	}
}

// Name 返回采集器唯一标识名称
func (c *nvramCollector) Name() string {
	return "nvram"
}

// Update 执行 NVRAM 指标抓取流程并推送指标
func (c *nvramCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := c.client.GetAll(ctx, nvramRequiredKeys)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("批量读取 NVRAM 变量失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 1. router_info
	productID := strings.TrimSpace(data["productid"])
	if productID == "" {
		productID = "unknown"
	}
	firmwareVersion := formatFirmwareVersion(data["buildno"], data["extendno"])
	ch <- prometheus.MustNewConstMetric(
		routerInfoDesc,
		prometheus.GaugeValue,
		1.0,
		productID,
		firmwareVersion,
		c.architecture,
	)

	// 2. router_network_mode
	mode := parseNetworkMode(data["sw_mode"])
	ch <- prometheus.MustNewConstMetric(
		routerNetworkModeDesc,
		prometheus.GaugeValue,
		1.0,
		mode,
	)

	// 3. router_gateway_info
	lanIP := strings.TrimSpace(data["lan_ipaddr"])
	lanNetmask := strings.TrimSpace(data["lan_netmask"])
	wanIP := strings.TrimSpace(data["wan0_ipaddr"])
	wanGateway := strings.TrimSpace(data["wan0_gateway"])
	wanDNS := strings.TrimSpace(data["wan0_dns"])
	ch <- prometheus.MustNewConstMetric(
		routerGatewayInfoDesc,
		prometheus.GaugeValue,
		1.0,
		lanIP,
		lanNetmask,
		wanIP,
		wanGateway,
		wanDNS,
	)

	// 4. router_wan_status (wan0_state_t == "2" 标识正常连通)
	wanStatus := 0.0
	if strings.TrimSpace(data["wan0_state_t"]) == "2" {
		wanStatus = 1.0
	}
	wanProto := strings.TrimSpace(data["wan0_proto"])
	ch <- prometheus.MustNewConstMetric(
		routerWanStatusDesc,
		prometheus.GaugeValue,
		wanStatus,
		"wan0",
		wanIP,
		wanProto,
	)

	// 5. router_wan_internet_status (link_internet == "2" 标识外网正常连通)
	internetStatus := 0.0
	if strings.TrimSpace(data["link_internet"]) == "2" {
		internetStatus = 1.0
	}
	ch <- prometheus.MustNewConstMetric(
		routerWanInternetStatusDesc,
		prometheus.GaugeValue,
		internetStatus,
	)

	// 6. router_wan_uptime_seconds (WAN 口在线秒数)
	var wanUptime float64
	if rawUptime := strings.TrimSpace(data["wan0_uptime"]); rawUptime != "" {
		if parsed, err := strconv.ParseFloat(rawUptime, 64); err == nil && parsed >= 0 {
			wanUptime = parsed
		}
	}
	ch <- prometheus.MustNewConstMetric(
		routerWanUptimeDesc,
		prometheus.GaugeValue,
		wanUptime,
		"wan0",
	)

	// 7. router_ntp_synced (ntp_ready == "1" 标识时钟已完成同步)
	ntpSynced := 0.0
	if strings.TrimSpace(data["ntp_ready"]) == "1" {
		ntpSynced = 1.0
	}
	ch <- prometheus.MustNewConstMetric(
		routerNtpSyncedDesc,
		prometheus.GaugeValue,
		ntpSynced,
	)

	return nil
}

// formatFirmwareVersion 根据 buildno 与 extendno 拼接固件完整版本字符串
func formatFirmwareVersion(buildno, extendno string) string {
	b := strings.TrimSpace(buildno)
	e := strings.TrimSpace(extendno)

	if b == "" && e == "" {
		return "unknown"
	}
	if b == "" {
		return e
	}
	if e == "" {
		return b
	}

	if strings.HasPrefix(e, "_") {
		return b + e
	}
	return b + "_" + e
}

// parseNetworkMode 将 NVRAM 中的 sw_mode 转换为标准化模式字符串
func parseNetworkMode(swMode string) string {
	switch strings.TrimSpace(swMode) {
	case "1":
		return "router"
	case "2":
		return "repeater"
	case "3":
		return "ap"
	case "4":
		return "media_bridge"
	default:
		return "unknown"
	}
}
