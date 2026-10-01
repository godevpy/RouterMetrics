// Package collector 实现各类系统的 Prometheus 指标采集器。
package collector

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"metrics/pkg/nvram"
	"metrics/pkg/util"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// cpuTempDesc 描述 CPU 核心温度指标 (单位: 摄氏度)
	cpuTempDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "cpu", "temperature_celsius"),
		"CPU temperature in Celsius.",
		[]string{"zone", "type"}, nil,
	)

	// wifiTempDesc 描述各 Wi-Fi 频段无线芯片温度指标 (单位: 摄氏度)
	wifiTempDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "wifi", "temperature_celsius"),
		"Wi-Fi temperature in Celsius.",
		[]string{"interface", "band"}, nil,
	)
)

// phyTempsenseRegex 用于提取 wl phy_tempsense 输出的首个有效温度数字（支持负数与小数）
var phyTempsenseRegex = regexp.MustCompile(`(?m)(?:^|\s)([-+]?[0-9]+(?:\.[0-9]+)?)(?:\s|\(|$)`)

// wifiBandMapping 定义 NVRAM 网卡变量与 Wi-Fi 频段的映射关系
type wifiBandMapping struct {
	nvramKey string
	band     string
}

// defaultWifiBands 预设的主流华硕机型双频/三频网卡 NVRAM 键值
var defaultWifiBands = []wifiBandMapping{
	{nvramKey: "wl0_ifname", band: "2.4GHz"},
	{nvramKey: "wl1_ifname", band: "5GHz"},
	{nvramKey: "wl2_ifname", band: "5GHz-2/6GHz"},
}

// thermalCollector 收集 CPU 硬件及无线芯片温度指标
type thermalCollector struct {
	sysPath     string
	nvramClient nvram.Client
	cmdRunner   func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error)
	cmdTimeout  time.Duration
}

// RegisterThermalCollector 将硬件温度采集器注册至采集器管理器
func RegisterThermalCollector() {
	RegisterCollector("thermal", true, NewThermalCollector)
}

func init() {
	RegisterThermalCollector()
}

// NewThermalCollector 创建使用系统默认路径及外部命令的温度采集器
func NewThermalCollector() (Collector, error) {
	return NewThermalCollectorWithConfig(
		"/sys",
		nvram.NewClient(800*time.Millisecond),
		util.RunCommandWithTimeout,
	), nil
}

// NewThermalCollectorWithConfig 支持依赖注入的温度采集器构造函数（用于单元测试与模拟环境）
func NewThermalCollectorWithConfig(
	sysPath string,
	nvramClient nvram.Client,
	cmdRunner func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error),
) Collector {
	if sysPath == "" {
		sysPath = "/sys"
	}
	if nvramClient == nil {
		nvramClient = nvram.NewClient(800 * time.Millisecond)
	}
	if cmdRunner == nil {
		cmdRunner = util.RunCommandWithTimeout
	}
	return &thermalCollector{
		sysPath:     sysPath,
		nvramClient: nvramClient,
		cmdRunner:   cmdRunner,
		cmdTimeout:  800 * time.Millisecond,
	}
}

// Name 返回采集器标识名称
func (c *thermalCollector) Name() string {
	return "thermal"
}

// Update 执行指标抓取流程，集成超时保护与单点故障隔离逻辑
func (c *thermalCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// 1. 采集 CPU 温度 (/sys/class/thermal/thermal_zone*)
	if err := c.collectCPUTemps(ctx, ch); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 采集 Wi-Fi 无线芯片温度 (wl -i <iface> phy_tempsense)
	if err := c.collectWifiTemps(ctx, ch); err != nil {
		return err
	}

	return nil
}

// collectCPUTemps 扫描 sysfs 中的散热区并读取温度与类型
func (c *thermalCollector) collectCPUTemps(ctx context.Context, ch chan<- prometheus.Metric) error {
	pattern := filepath.Join(c.sysPath, "class", "thermal", "thermal_zone*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("扫描 CPU 散热区目录失败: %w", err)
	}

	sort.Strings(matches)

	for _, zoneDir := range matches {
		if err := ctx.Err(); err != nil {
			return err
		}

		base := filepath.Base(zoneDir)
		zone := strings.TrimPrefix(base, "thermal_zone")
		if zone == "" || zone == base {
			zone = base
		}

		tempPath := filepath.Join(zoneDir, "temp")
		tempBytes, err := os.ReadFile(tempPath)
		if err != nil {
			log.Printf("[WARN] 读取 CPU 散热区温度文件 %s 失败: %v", tempPath, err)
			continue
		}

		tempMilli, err := strconv.ParseFloat(strings.TrimSpace(string(tempBytes)), 64)
		if err != nil {
			log.Printf("[WARN] 解析 CPU 温度数值 %s (%s) 失败: %v", string(tempBytes), tempPath, err)
			continue
		}
		tempCelsius := tempMilli / 1000.0

		zoneType := "unknown"
		typePath := filepath.Join(zoneDir, "type")
		if typeBytes, err := os.ReadFile(typePath); err == nil {
			if t := strings.TrimSpace(string(typeBytes)); t != "" {
				zoneType = t
			}
		}

		ch <- prometheus.MustNewConstMetric(cpuTempDesc, prometheus.GaugeValue, tempCelsius, zone, zoneType)
	}

	return nil
}

// collectWifiTemps 通过 NVRAM 获取各频段网卡接口并执行 wl phy_tempsense
func (c *thermalCollector) collectWifiTemps(ctx context.Context, ch chan<- prometheus.Metric) error {
	if c.nvramClient == nil {
		return nil
	}

	keys := make([]string, len(defaultWifiBands))
	for i, b := range defaultWifiBands {
		keys[i] = b.nvramKey
	}

	ifaceMap, err := c.nvramClient.GetAll(ctx, keys)
	if err != nil {
		log.Printf("[WARN] 查询 NVRAM Wi-Fi 网卡配置失败: %v", err)
		return nil
	}

	for _, b := range defaultWifiBands {
		if err := ctx.Err(); err != nil {
			return err
		}

		iface := strings.TrimSpace(ifaceMap[b.nvramKey])
		if iface == "" {
			continue
		}

		out, err := c.cmdRunner(ctx, c.cmdTimeout, "wl", "-i", iface, "phy_tempsense")
		if err != nil {
			log.Printf("[WARN] 采集 Wi-Fi 接口 %s (%s) 温度失败: %v", iface, b.band, err)
			continue
		}

		temp, err := parsePhyTempsense(string(out))
		if err != nil {
			log.Printf("[WARN] 解析 Wi-Fi 接口 %s (%s) 温度输出失败: %v", iface, b.band, err)
			continue
		}

		ch <- prometheus.MustNewConstMetric(wifiTempDesc, prometheus.GaugeValue, temp, iface, b.band)
	}

	return nil
}

// parsePhyTempsense 解析 wl phy_tempsense 输出，提取首个有效温度数值
func parsePhyTempsense(output string) (float64, error) {
	str := strings.TrimSpace(output)
	if str == "" {
		return 0, fmt.Errorf("phy_tempsense 输出为空")
	}

	matches := phyTempsenseRegex.FindStringSubmatch(str)
	if len(matches) < 2 {
		return 0, fmt.Errorf("无法从 phy_tempsense 输出中解析有效温度: %q", output)
	}

	temp, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("解析温度数值 %q 失败: %w", matches[1], err)
	}

	return temp, nil
}
