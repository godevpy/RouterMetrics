package collector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestParseLoadavg 验证 /proc/loadavg 数据流解析
func TestParseLoadavg(t *testing.T) {
	t.Run("正常格式解析", func(t *testing.T) {
		content := "0.45 0.32 0.28 1/142 12345\n"
		l1, l5, l15, err := parseLoadavg(strings.NewReader(content))
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if l1 != 0.45 || l5 != 0.32 || l15 != 0.28 {
			t.Fatalf("解析值不符合预期: %v, %v, %v", l1, l5, l15)
		}
	})

	t.Run("字段不足报错", func(t *testing.T) {
		content := "0.45 0.32\n"
		_, _, _, err := parseLoadavg(strings.NewReader(content))
		if err == nil {
			t.Fatalf("期望字段不足时报错，但返回了 nil")
		}
	})

	t.Run("非数值类型报错", func(t *testing.T) {
		content := "invalid 0.32 0.28 1/142 12345\n"
		_, _, _, err := parseLoadavg(strings.NewReader(content))
		if err == nil {
			t.Fatalf("期望非数值内容报错，但返回了 nil")
		}
	})

	t.Run("空内容报错", func(t *testing.T) {
		_, _, _, err := parseLoadavg(strings.NewReader(""))
		if err == nil {
			t.Fatalf("期望空内容报错，但返回了 nil")
		}
	})
}

// TestParseMeminfo 验证 /proc/meminfo 数据流解析与单位换算
func TestParseMeminfo(t *testing.T) {
	t.Run("正常格式解析与kB换算", func(t *testing.T) {
		content := `MemTotal:         508824 kB
MemFree:          123456 kB
MemAvailable:     234567 kB
Buffers:           12000 kB
Cached:            98765 kB
`
		mem, err := parseMeminfo(strings.NewReader(content))
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if mem["MemTotal"] != 508824*1024 || mem["MemFree"] != 123456*1024 {
			t.Fatalf("内存换算异常: %v", mem)
		}
		if mem["MemAvailable"] != 234567*1024 {
			t.Fatalf("MemAvailable 换算异常: %v", mem["MemAvailable"])
		}
		if mem["Buffers"] != 12000*1024 {
			t.Fatalf("Buffers 换算异常: %v", mem["Buffers"])
		}
		if mem["Cached"] != 98765*1024 {
			t.Fatalf("Cached 换算异常: %v", mem["Cached"])
		}
	})

	t.Run("多单位兼容换算", func(t *testing.T) {
		content := `MemTotal:         1024 MB
SwapTotal:        1 GB
DirectBytes:      100 B
NoUnit:           42
`
		mem, err := parseMeminfo(strings.NewReader(content))
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if mem["MemTotal"] != 1024*1024*1024 {
			t.Fatalf("MB 换算异常: %v", mem["MemTotal"])
		}
		if mem["SwapTotal"] != 1*1024*1024*1024 {
			t.Fatalf("GB 换算异常: %v", mem["SwapTotal"])
		}
		if mem["DirectBytes"] != 100 {
			t.Fatalf("B 换算异常: %v", mem["DirectBytes"])
		}
		if mem["NoUnit"] != 42 {
			t.Fatalf("无单位换算异常: %v", mem["NoUnit"])
		}
	})

	t.Run("空内容报错", func(t *testing.T) {
		_, err := parseMeminfo(strings.NewReader(""))
		if err == nil {
			t.Fatalf("期望空内容报错，但返回了 nil")
		}
	})
}

// TestParseUptime 验证 /proc/uptime 数据流解析
func TestParseUptime(t *testing.T) {
	t.Run("正常双字段解析", func(t *testing.T) {
		content := "350735.65 1402945.32\n"
		uptime, err := parseUptime(strings.NewReader(content))
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if uptime != 350735.65 {
			t.Fatalf("uptime 解析值不符合预期: %v", uptime)
		}
	})

	t.Run("单字段解析", func(t *testing.T) {
		content := "12345.67\n"
		uptime, err := parseUptime(strings.NewReader(content))
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if uptime != 12345.67 {
			t.Fatalf("uptime 解析值不符合预期: %v", uptime)
		}
	})

	t.Run("非数值类型报错", func(t *testing.T) {
		content := "invalid_uptime 12345\n"
		_, err := parseUptime(strings.NewReader(content))
		if err == nil {
			t.Fatalf("期望非数值 uptime 报错，但返回了 nil")
		}
	})

	t.Run("空内容报错", func(t *testing.T) {
		_, err := parseUptime(strings.NewReader(""))
		if err == nil {
			t.Fatalf("期望空内容报错，但返回了 nil")
		}
	})
}

// TestSystemCollector_Update 验证系统采集器抓取流程及指标生成完整性
func TestSystemCollector_Update(t *testing.T) {
	// 创建临时 mock proc 目录
	tempDir := t.TempDir()

	loadavgContent := "1.25 2.50 3.75 2/100 9999\n"
	meminfoContent := `MemTotal:        1024000 kB
MemFree:          512000 kB
MemAvailable:     768000 kB
Buffers:           64000 kB
Cached:           128000 kB
`
	uptimeContent := "86400.50 172800.00\n"

	if err := os.WriteFile(filepath.Join(tempDir, "loadavg"), []byte(loadavgContent), 0644); err != nil {
		t.Fatalf("写入 mock loadavg 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "meminfo"), []byte(meminfoContent), 0644); err != nil {
		t.Fatalf("写入 mock meminfo 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "uptime"), []byte(uptimeContent), 0644); err != nil {
		t.Fatalf("写入 mock uptime 失败: %v", err)
	}

	col := NewSystemCollectorWithProc(tempDir)
	if col.Name() != "system" {
		t.Fatalf("采集器名称期望为 system，实际为: %s", col.Name())
	}

	ch := make(chan prometheus.Metric, 20)
	if err := col.Update(context.Background(), ch); err != nil {
		t.Fatalf("采集器 Update 执行失败: %v", err)
	}
	close(ch)

	metrics := make(map[string]float64)
	metricTypes := make(map[string]dto.MetricType)

	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("写入 DTO 指标失败: %v", err)
		}

		descStr := m.Desc().String()
		if strings.Contains(descStr, "router_load1\"") {
			metrics["router_load1"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_load1"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_load5\"") {
			metrics["router_load5"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_load5"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_load15\"") {
			metrics["router_load15"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_load15"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_memory_total_bytes") {
			metrics["router_memory_total_bytes"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_memory_total_bytes"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_memory_free_bytes") {
			metrics["router_memory_free_bytes"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_memory_free_bytes"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_memory_available_bytes") {
			metrics["router_memory_available_bytes"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_memory_available_bytes"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_memory_buffers_bytes") {
			metrics["router_memory_buffers_bytes"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_memory_buffers_bytes"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_memory_cached_bytes") {
			metrics["router_memory_cached_bytes"] = dtoMetric.GetGauge().GetValue()
			metricTypes["router_memory_cached_bytes"] = dto.MetricType_GAUGE
		} else if strings.Contains(descStr, "router_uptime_seconds") {
			metrics["router_uptime_seconds"] = dtoMetric.GetCounter().GetValue()
			metricTypes["router_uptime_seconds"] = dto.MetricType_COUNTER
		}
	}

	expectedGauges := map[string]float64{
		"router_load1":                  1.25,
		"router_load5":                  2.50,
		"router_load15":                 3.75,
		"router_memory_total_bytes":     1024000 * 1024,
		"router_memory_free_bytes":      512000 * 1024,
		"router_memory_available_bytes": 768000 * 1024,
		"router_memory_buffers_bytes":   64000 * 1024,
		"router_memory_cached_bytes":    128000 * 1024,
	}

	for k, expectedVal := range expectedGauges {
		val, ok := metrics[k]
		if !ok {
			t.Errorf("缺少期望指标: %s", k)
			continue
		}
		if val != expectedVal {
			t.Errorf("指标 %s 数值不符，期望: %v, 实际: %v", k, expectedVal, val)
		}
		if metricTypes[k] != dto.MetricType_GAUGE {
			t.Errorf("指标 %s 类型不符，期望 GAUGE，实际: %v", k, metricTypes[k])
		}
	}

	uptimeVal, ok := metrics["router_uptime_seconds"]
	if !ok {
		t.Fatalf("缺少 router_uptime_seconds 指标")
	}
	if uptimeVal != 86400.50 {
		t.Errorf("router_uptime_seconds 数值不符，期望 86400.50，实际: %v", uptimeVal)
	}
	if metricTypes["router_uptime_seconds"] != dto.MetricType_COUNTER {
		t.Errorf("router_uptime_seconds 类型不符，期望 COUNTER，实际: %v", metricTypes["router_uptime_seconds"])
	}
}

// TestSystemCollector_Update_ContextCanceled 验证 Context 超时或取消时及时退出
func TestSystemCollector_Update_ContextCanceled(t *testing.T) {
	tempDir := t.TempDir()
	col := NewSystemCollectorWithProc(tempDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	ch := make(chan prometheus.Metric, 10)
	err := col.Update(ctx, ch)
	if err == nil {
		t.Fatalf("期望已取消的 Context 返回错误，但得到 nil")
	}
}

// TestSystemCollector_Update_FileErrors 验证缺失文件时返回明确错误
func TestSystemCollector_Update_FileErrors(t *testing.T) {
	t.Run("不存在的 proc 路径", func(t *testing.T) {
		col := NewSystemCollectorWithProc("/path/that/does/not/exist")
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err == nil {
			t.Fatalf("期望不存在路径返回错误，但得到 nil")
		}
	})

	t.Run("缺失 meminfo 文件", func(t *testing.T) {
		tempDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(tempDir, "loadavg"), []byte("0.1 0.2 0.3 1/10 100\n"), 0644)
		col := NewSystemCollectorWithProc(tempDir)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err == nil {
			t.Fatalf("期望缺失 meminfo 时返回错误，但得到 nil")
		}
	})

	t.Run("缺失 uptime 文件", func(t *testing.T) {
		tempDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(tempDir, "loadavg"), []byte("0.1 0.2 0.3 1/10 100\n"), 0644)
		_ = os.WriteFile(filepath.Join(tempDir, "meminfo"), []byte("MemTotal: 1000 kB\n"), 0644)
		col := NewSystemCollectorWithProc(tempDir)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err == nil {
			t.Fatalf("期望缺失 uptime 时返回错误，但得到 nil")
		}
	})
}

// TestSystemCollector_Registration 验证系统采集器注册中心状态及工厂函数
func TestSystemCollector_Registration(t *testing.T) {
	RegisterSystemCollector()

	avail := AvailableCollectors()
	if enabled, ok := avail["system"]; !ok || !enabled {
		t.Fatalf("期望 system 采集器已注册且默认启用，实际 enabled: %v, ok: %v", enabled, ok)
	}

	factoriesMu.RLock()
	factory, ok := factories["system"]
	factoriesMu.RUnlock()

	if !ok {
		t.Fatalf("未在 factories 表中找到 system 工厂函数")
	}

	col, err := factory()
	if err != nil {
		t.Fatalf("调用 system 工厂函数失败: %v", err)
	}
	if col == nil || col.Name() != "system" {
		t.Fatalf("创建的采集器实例异常: %v", col)
	}
}
