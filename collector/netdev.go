// Package collector 实现各类系统的 Prometheus 指标采集器。
package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	rxBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "network", "receive_bytes_total"),
		"Network device receive bytes total.",
		[]string{"device"}, nil,
	)
	txBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "network", "transmit_bytes_total"),
		"Network device transmit bytes total.",
		[]string{"device"}, nil,
	)
	rxPacketsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "network", "receive_packets_total"),
		"Network device receive packets total.",
		[]string{"device"}, nil,
	)
	txPacketsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "network", "transmit_packets_total"),
		"Network device transmit packets total.",
		[]string{"device"}, nil,
	)
	rxErrorsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "network", "receive_errors_total"),
		"Network device receive errors total.",
		[]string{"device"}, nil,
	)
	txErrorsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "network", "transmit_errors_total"),
		"Network device transmit errors total.",
		[]string{"device"}, nil,
	)
)

var (
	// defaultNetdevExcludePattern 默认排除的回环网卡正则
	defaultNetdevExcludePattern = "^(lo)$"
)

// NetDevStats 包含单个网络接口的关键收发统计指标
type NetDevStats struct {
	RxBytes   float64
	RxPackets float64
	RxErrors  float64
	TxBytes   float64
	TxPackets float64
	TxErrors  float64
}

// netdevCollector 收集网络接口的收发流量与数据包统计
type netdevCollector struct {
	procPath      string
	includeRegexp *regexp.Regexp
	excludeRegexp *regexp.Regexp
}

// RegisterNetdevCollector 将网络接口流量采集器注册至采集器管理器
func RegisterNetdevCollector() {
	RegisterCollector("netdev", true, NewNetdevCollector)
}

func init() {
	RegisterNetdevCollector()
}

// NewNetdevCollector 创建使用默认 /proc 路径及默认排除回环网卡 (^(lo)$) 的网络接口流量采集器
func NewNetdevCollector() (Collector, error) {
	excludeRegex, err := regexp.Compile(defaultNetdevExcludePattern)
	if err != nil {
		return nil, fmt.Errorf("编译 netdev 默认排除正则表达式失败: %w", err)
	}
	return NewNetdevCollectorWithProc("/proc", nil, excludeRegex), nil
}

// NewNetdevCollectorWithProc 创建带有自定义 proc 路径及过滤正则的网络接口流量采集器（便于依赖注入测试）
func NewNetdevCollectorWithProc(procPath string, includeRegexp *regexp.Regexp, excludeRegexp *regexp.Regexp) Collector {
	return &netdevCollector{
		procPath:      procPath,
		includeRegexp: includeRegexp,
		excludeRegexp: excludeRegexp,
	}
}

// Name 返回采集器标识名称
func (c *netdevCollector) Name() string {
	return "netdev"
}

// shouldInclude 根据配置的包含与排除正则判断是否抓取该网卡
func (c *netdevCollector) shouldInclude(device string) bool {
	if c.excludeRegexp != nil && c.excludeRegexp.MatchString(device) {
		return false
	}
	if c.includeRegexp != nil && !c.includeRegexp.MatchString(device) {
		return false
	}
	return true
}

// Update 执行网络接口指标抓取并推送至 Prometheus 指标通道
func (c *netdevCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	devPath := filepath.Join(c.procPath, "net", "dev")
	file, err := os.Open(devPath)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", devPath, err)
	}
	defer file.Close()

	statsMap, err := parseNetDev(file)
	if err != nil {
		return fmt.Errorf("解析 %s 失败: %w", devPath, err)
	}

	// 排序接口名称以保证输出确定性
	devs := make([]string, 0, len(statsMap))
	for dev := range statsMap {
		if !c.shouldInclude(dev) {
			continue
		}
		devs = append(devs, dev)
	}
	sort.Strings(devs)

	for _, dev := range devs {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats := statsMap[dev]
		ch <- prometheus.MustNewConstMetric(rxBytesDesc, prometheus.CounterValue, stats.RxBytes, dev)
		ch <- prometheus.MustNewConstMetric(txBytesDesc, prometheus.CounterValue, stats.TxBytes, dev)
		ch <- prometheus.MustNewConstMetric(rxPacketsDesc, prometheus.CounterValue, stats.RxPackets, dev)
		ch <- prometheus.MustNewConstMetric(txPacketsDesc, prometheus.CounterValue, stats.TxPackets, dev)
		ch <- prometheus.MustNewConstMetric(rxErrorsDesc, prometheus.CounterValue, stats.RxErrors, dev)
		ch <- prometheus.MustNewConstMetric(txErrorsDesc, prometheus.CounterValue, stats.TxErrors, dev)
	}

	return nil
}

// parseNetDev 从 io.Reader 流式解析 /proc/net/dev 数据
func parseNetDev(r io.Reader) (map[string]NetDevStats, error) {
	scanner := bufio.NewScanner(r)
	statsMap := make(map[string]NetDevStats)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			// 表头行 (如 Inter-| Receive ... 或 face |bytes ...)
			continue
		}

		dev := strings.TrimSpace(line[:colonIdx])
		if dev == "" {
			continue
		}

		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 16 {
			return nil, fmt.Errorf("网卡 %s 格式异常: 字段数量不足 (%d < 16)", dev, len(fields))
		}

		rxBytes, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("解析网卡 %s 接收字节数失败: %w", dev, err)
		}
		rxPackets, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("解析网卡 %s 接收数据包数失败: %w", dev, err)
		}
		rxErrors, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return nil, fmt.Errorf("解析网卡 %s 接收错误数失败: %w", dev, err)
		}

		txBytes, err := strconv.ParseFloat(fields[8], 64)
		if err != nil {
			return nil, fmt.Errorf("解析网卡 %s 发送字节数失败: %w", dev, err)
		}
		txPackets, err := strconv.ParseFloat(fields[9], 64)
		if err != nil {
			return nil, fmt.Errorf("解析网卡 %s 发送数据包数失败: %w", dev, err)
		}
		txErrors, err := strconv.ParseFloat(fields[10], 64)
		if err != nil {
			return nil, fmt.Errorf("解析网卡 %s 发送错误数失败: %w", dev, err)
		}

		statsMap[dev] = NetDevStats{
			RxBytes:   rxBytes,
			RxPackets: rxPackets,
			RxErrors:  rxErrors,
			TxBytes:   txBytes,
			TxPackets: txPackets,
			TxErrors:  txErrors,
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 netdev 数据流失败: %w", err)
	}

	if len(statsMap) == 0 {
		return nil, fmt.Errorf("/proc/net/dev 内容为空或未解析到有效接口数据")
	}

	return statsMap, nil
}
