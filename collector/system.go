// Package collector 实现各类系统的 Prometheus 指标采集器。
package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	load1Desc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "", "load1"),
		"1m system load average.",
		nil, nil,
	)
	load5Desc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "", "load5"),
		"5m system load average.",
		nil, nil,
	)
	load15Desc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "", "load15"),
		"15m system load average.",
		nil, nil,
	)

	memTotalDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "memory", "total_bytes"),
		"Total physical memory in bytes.",
		nil, nil,
	)
	memFreeDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "memory", "free_bytes"),
		"Free physical memory in bytes.",
		nil, nil,
	)
	memAvailableDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "memory", "available_bytes"),
		"Available physical memory in bytes.",
		nil, nil,
	)
	memBuffersDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "memory", "buffers_bytes"),
		"Buffers memory in bytes.",
		nil, nil,
	)
	memCachedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "memory", "cached_bytes"),
		"Cached memory in bytes.",
		nil, nil,
	)

	uptimeDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "", "uptime_seconds"),
		"System uptime in seconds.",
		nil, nil,
	)
)

// systemCollector 收集系统负载、内存及运行时间等基础系统指标
type systemCollector struct {
	procPath string
}

// RegisterSystemCollector 将系统采集器注册至采集器管理器
func RegisterSystemCollector() {
	RegisterCollector("system", true, NewSystemCollector)
}

func init() {
	RegisterSystemCollector()
}

// NewSystemCollector 创建使用默认 /proc 路径的系统采集器工厂
func NewSystemCollector() (Collector, error) {
	return NewSystemCollectorWithProc("/proc"), nil
}

// NewSystemCollectorWithProc 创建带有指定 proc 路径的系统采集器（方便单元测试注入）
func NewSystemCollectorWithProc(procPath string) Collector {
	return &systemCollector{
		procPath: procPath,
	}
}

// Name 返回采集器标识名称
func (c *systemCollector) Name() string {
	return "system"
}

// Update 执行指标抓取流程并推送指标
func (c *systemCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// 1. 采集 /proc/loadavg
	loadavgPath := filepath.Join(c.procPath, "loadavg")
	loadavgFile, err := os.Open(loadavgPath)
	if err != nil {
		return fmt.Errorf("打开 loadavg 失败: %w", err)
	}
	l1, l5, l15, err := parseLoadavg(loadavgFile)
	loadavgFile.Close()
	if err != nil {
		return fmt.Errorf("解析 loadavg 失败: %w", err)
	}

	ch <- prometheus.MustNewConstMetric(load1Desc, prometheus.GaugeValue, l1)
	ch <- prometheus.MustNewConstMetric(load5Desc, prometheus.GaugeValue, l5)
	ch <- prometheus.MustNewConstMetric(load15Desc, prometheus.GaugeValue, l15)

	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 采集 /proc/meminfo
	meminfoPath := filepath.Join(c.procPath, "meminfo")
	meminfoFile, err := os.Open(meminfoPath)
	if err != nil {
		return fmt.Errorf("打开 meminfo 失败: %w", err)
	}
	mem, err := parseMeminfo(meminfoFile)
	meminfoFile.Close()
	if err != nil {
		return fmt.Errorf("解析 meminfo 失败: %w", err)
	}

	if v, ok := mem["MemTotal"]; ok {
		ch <- prometheus.MustNewConstMetric(memTotalDesc, prometheus.GaugeValue, v)
	}
	if v, ok := mem["MemFree"]; ok {
		ch <- prometheus.MustNewConstMetric(memFreeDesc, prometheus.GaugeValue, v)
	}
	if v, ok := mem["MemAvailable"]; ok {
		ch <- prometheus.MustNewConstMetric(memAvailableDesc, prometheus.GaugeValue, v)
	}
	if v, ok := mem["Buffers"]; ok {
		ch <- prometheus.MustNewConstMetric(memBuffersDesc, prometheus.GaugeValue, v)
	}
	if v, ok := mem["Cached"]; ok {
		ch <- prometheus.MustNewConstMetric(memCachedDesc, prometheus.GaugeValue, v)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 3. 采集 /proc/uptime
	uptimePath := filepath.Join(c.procPath, "uptime")
	uptimeFile, err := os.Open(uptimePath)
	if err != nil {
		return fmt.Errorf("打开 uptime 失败: %w", err)
	}
	uptime, err := parseUptime(uptimeFile)
	uptimeFile.Close()
	if err != nil {
		return fmt.Errorf("解析 uptime 失败: %w", err)
	}

	ch <- prometheus.MustNewConstMetric(uptimeDesc, prometheus.CounterValue, uptime)

	return nil
}

// parseLoadavg 从 io.Reader 流式解析 loadavg 数据 (1m, 5m, 15m)
func parseLoadavg(r io.Reader) (float64, float64, float64, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, 0, 0, err
		}
		return 0, 0, 0, fmt.Errorf("loadavg 内容为空")
	}

	fields := strings.Fields(scanner.Text())
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("loadavg 格式异常: 字段数量不足 (%d < 3)", len(fields))
	}

	l1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("解析 load1 失败: %w", err)
	}

	l5, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("解析 load5 失败: %w", err)
	}

	l15, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("解析 load15 失败: %w", err)
	}

	return l1, l5, l15, nil
}

// parseMeminfo 从 io.Reader 流式解析 meminfo 数据并统一转换为字节数 (bytes)
func parseMeminfo(r io.Reader) (map[string]float64, error) {
	scanner := bufio.NewScanner(r)
	res := make(map[string]float64)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		valFields := strings.Fields(strings.TrimSpace(parts[1]))
		if len(valFields) == 0 {
			continue
		}

		val, err := strconv.ParseFloat(valFields[0], 64)
		if err != nil {
			continue
		}

		// 根据单位转换为标准字节数 (bytes)
		if len(valFields) > 1 {
			switch strings.ToLower(valFields[1]) {
			case "kb", "kib":
				val *= 1024
			case "mb", "mib":
				val *= 1024 * 1024
			case "gb", "gib":
				val *= 1024 * 1024 * 1024
			}
		}

		res[key] = val
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, fmt.Errorf("meminfo 内容为空或未解析到有效字段")
	}

	return res, nil
}

// parseUptime 从 io.Reader 流式解析 uptime 数据 (累计运行秒数)
func parseUptime(r io.Reader) (float64, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("uptime 内容为空")
	}

	fields := strings.Fields(scanner.Text())
	if len(fields) < 1 {
		return 0, fmt.Errorf("uptime 格式异常: 无有效字段")
	}

	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("解析 uptime 失败: %w", err)
	}

	return uptime, nil
}
