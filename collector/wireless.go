// Package collector 实现各类系统的 Prometheus 指标采集器。
package collector

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"

	"metrics/pkg/nvram"
	"metrics/pkg/util"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// wifiConnectedClientsDesc 描述各 Wi-Fi 频段当前关联连接的无线客户端设备总数
	wifiConnectedClientsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "wifi", "connected_clients"),
		"Number of connected Wi-Fi clients.",
		[]string{"interface", "band", "ssid"}, nil,
	)

	// wifiRadioEnabledDesc 描述各 Wi-Fi 频段无线射频的启用开关状态 (1 为开启，0 为关闭)
	wifiRadioEnabledDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "wifi", "radio_enabled"),
		"Wi-Fi radio enabled status (1 for enabled, 0 for disabled).",
		[]string{"interface", "band"}, nil,
	)
)

// macRegex 用于精准提取单行输出中的有效 6 字节 MAC 地址
var macRegex = regexp.MustCompile(`(?i)(?:^|\s)(?:assoclist\s+)?([0-9a-f]{2}(?::[0-9a-f]{2}){5})(?:\s|$)`)

// wifiBandInfo 定义 NVRAM 键名与无线频段的映射关系
type wifiBandInfo struct {
	ifnameKey string
	radioKey  string
	ssidKey   string
	band      string
}

// defaultWirelessBands 预设的主流华硕双频/三频机型无线频段映射表
var defaultWirelessBands = []wifiBandInfo{
	{ifnameKey: "wl0_ifname", radioKey: "wl0_radio", ssidKey: "wl0_ssid", band: "2.4GHz"},
	{ifnameKey: "wl1_ifname", radioKey: "wl1_radio", ssidKey: "wl1_ssid", band: "5GHz-1"},
	{ifnameKey: "wl2_ifname", radioKey: "wl2_radio", ssidKey: "wl2_ssid", band: "5GHz-2 / 6GHz"},
}

// wirelessCollector 收集无线射频开关状态及关联客户端数量指标
type wirelessCollector struct {
	nvramClient nvram.Client
	cmdRunner   func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error)
	cmdTimeout  time.Duration
}

// RegisterWirelessCollector 将无线状态采集器注册至注册中心
func RegisterWirelessCollector() {
	RegisterCollector("wireless", true, NewWirelessCollector)
}

func init() {
	RegisterWirelessCollector()
}

// NewWirelessCollector 创建默认配置的无线状态采集器
func NewWirelessCollector() (Collector, error) {
	return NewWirelessCollectorWithConfig(
		nvram.NewClient(800*time.Millisecond),
		util.RunCommandWithTimeout,
	), nil
}

// NewWirelessCollectorWithConfig 支持依赖注入的无线采集器构造函数（用于单元测试与模拟环境）
func NewWirelessCollectorWithConfig(
	nvramClient nvram.Client,
	cmdRunner func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error),
) Collector {
	if nvramClient == nil {
		nvramClient = nvram.NewClient(800 * time.Millisecond)
	}
	if cmdRunner == nil {
		cmdRunner = util.RunCommandWithTimeout
	}
	return &wirelessCollector{
		nvramClient: nvramClient,
		cmdRunner:   cmdRunner,
		cmdTimeout:  800 * time.Millisecond,
	}
}

// Name 返回采集器唯一标识名称
func (c *wirelessCollector) Name() string {
	return "wireless"
}

// Update 执行指标抓取流程，集成超时保护与单接口故障隔离逻辑
func (c *wirelessCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if c.nvramClient == nil {
		return nil
	}

	// 组装所需查询的 NVRAM 变量键列表
	keys := make([]string, 0, len(defaultWirelessBands)*3)
	for _, b := range defaultWirelessBands {
		keys = append(keys, b.ifnameKey, b.radioKey, b.ssidKey)
	}

	nvramValues, err := c.nvramClient.GetAll(ctx, keys)
	if err != nil {
		log.Printf("[WARN] 查询 NVRAM Wi-Fi 配置失败: %v", err)
		return nil
	}

	for _, b := range defaultWirelessBands {
		if err := ctx.Err(); err != nil {
			return err
		}

		iface := strings.TrimSpace(nvramValues[b.ifnameKey])
		if iface == "" {
			continue
		}

		radioVal := strings.TrimSpace(nvramValues[b.radioKey])
		ssid := strings.TrimSpace(nvramValues[b.ssidKey])

		radioEnabled := 0.0
		if radioVal == "1" {
			radioEnabled = 1.0
		}

		// 暴露无线频段射频启用状态指标
		ch <- prometheus.MustNewConstMetric(wifiRadioEnabledDesc, prometheus.GaugeValue, radioEnabled, iface, b.band)

		// 射频开启时调用 wl assoclist 探测关联客户端；射频关闭时直接上报 0
		if radioEnabled == 1.0 {
			out, err := c.cmdRunner(ctx, c.cmdTimeout, "wl", "-i", iface, "assoclist")
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Printf("[WARN] 采集 Wi-Fi 接口 %s (%s) 客户端连接列表失败: %v", iface, b.band, err)
				continue
			}

			clients := parseAssoclist(string(out))
			ch <- prometheus.MustNewConstMetric(wifiConnectedClientsDesc, prometheus.GaugeValue, float64(len(clients)), iface, b.band, ssid)
		} else {
			ch <- prometheus.MustNewConstMetric(wifiConnectedClientsDesc, prometheus.GaugeValue, 0.0, iface, b.band, ssid)
		}
	}

	return nil
}

// parseAssoclist 解析 wl assoclist 输出并提取去重后的有效 MAC 地址列表
func parseAssoclist(output string) []string {
	var macs []string
	seen := make(map[string]struct{})
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		matches := macRegex.FindStringSubmatch(trimmed)
		if len(matches) >= 2 {
			mac := strings.ToLower(matches[1])
			if _, exists := seen[mac]; !exists {
				seen[mac] = struct{}{}
				macs = append(macs, mac)
			}
		}
	}

	if macs == nil {
		return []string{}
	}
	return macs
}
