package collector

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// sampleNetDevData 提供标准的 Linux /proc/net/dev 样例数据
const sampleNetDevData = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 40488661   229604    0    0    0     0          0         0 40488661   229604    0    0    0     0       0          0
  eth0: 12345678    12345    1    0    0     0          0         0 87654321    54321    2    0    0     0       0          0
   br0: 10293847    56789    0    0    0     0          0         0 98765432    98765    0    0    0     0       0          0
   wl0:  1122334     1122    0    0    0     0          0         0  4455667     4455    0    0    0     0       0          0
`

// TestParseNetDev 验证 /proc/net/dev 数据流解析与异常处理
func TestParseNetDev(t *testing.T) {
	t.Run("正常多网卡格式解析", func(t *testing.T) {
		stats, err := parseNetDev(strings.NewReader(sampleNetDevData))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}

		if len(stats) != 4 {
			t.Fatalf("期望解析到 4 个网卡，实际为 %d", len(stats))
		}

		eth0, ok := stats["eth0"]
		if !ok {
			t.Fatalf("未解析到 eth0 网卡")
		}
		if eth0.RxBytes != 12345678 || eth0.RxPackets != 12345 || eth0.RxErrors != 1 {
			t.Errorf("eth0 接收指标不符: %+v", eth0)
		}
		if eth0.TxBytes != 87654321 || eth0.TxPackets != 54321 || eth0.TxErrors != 2 {
			t.Errorf("eth0 发送指标不符: %+v", eth0)
		}

		lo, ok := stats["lo"]
		if !ok {
			t.Fatalf("未解析到 lo 网卡")
		}
		if lo.RxBytes != 40488661 || lo.TxBytes != 40488661 {
			t.Errorf("lo 流量指标不符: %+v", lo)
		}
	})

	t.Run("紧凑无空格冒号解析", func(t *testing.T) {
		// 某些内核版本在冒号后紧跟数字，无空格分隔
		content := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
eth1:1234 56 0 0 0 0 0 0 4321 65 0 0 0 0 0 0
`
		stats, err := parseNetDev(strings.NewReader(content))
		if err != nil {
			t.Fatalf("紧凑格式解析失败: %v", err)
		}
		eth1, ok := stats["eth1"]
		if !ok {
			t.Fatalf("未解析到 eth1")
		}
		if eth1.RxBytes != 1234 || eth1.RxPackets != 56 || eth1.TxBytes != 4321 || eth1.TxPackets != 65 {
			t.Errorf("eth1 数据解析不符: %+v", eth1)
		}
	})

	t.Run("空内容报错", func(t *testing.T) {
		_, err := parseNetDev(strings.NewReader(""))
		if err == nil {
			t.Fatalf("期望空内容返回错误，但返回了 nil")
		}
	})

	t.Run("仅有表头无数据行报错", func(t *testing.T) {
		content := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
`
		_, err := parseNetDev(strings.NewReader(content))
		if err == nil {
			t.Fatalf("期望仅表头内容返回错误，但返回了 nil")
		}
	})

	t.Run("字段数量不足报错", func(t *testing.T) {
		content := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
eth0: 123 456 0
`
		_, err := parseNetDev(strings.NewReader(content))
		if err == nil {
			t.Fatalf("期望字段不足时报错，但返回了 nil")
		}
	})

	t.Run("非数值字段报错", func(t *testing.T) {
		testCases := []struct {
			name    string
			content string
		}{
			{"rx_bytes 非数值", "eth0: inv 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15\n"},
			{"rx_packets 非数值", "eth0: 0 inv 2 3 4 5 6 7 8 9 10 11 12 13 14 15\n"},
			{"rx_errors 非数值", "eth0: 0 1 inv 3 4 5 6 7 8 9 10 11 12 13 14 15\n"},
			{"tx_bytes 非数值", "eth0: 0 1 2 3 4 5 6 7 inv 9 10 11 12 13 14 15\n"},
			{"tx_packets 非数值", "eth0: 0 1 2 3 4 5 6 7 8 inv 10 11 12 13 14 15\n"},
			{"tx_errors 非数值", "eth0: 0 1 2 3 4 5 6 7 8 9 inv 11 12 13 14 15\n"},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := parseNetDev(strings.NewReader(tc.content))
				if err == nil {
					t.Fatalf("期望 %s 报错，但返回了 nil", tc.name)
				}
			})
		}
	})

	t.Run("包含注释行与空接口名容错", func(t *testing.T) {
		content := `# 这是一个注释
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed

: 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16
eth0: 100 10 0 0 0 0 0 0 200 20 0 0 0 0 0 0
`
		stats, err := parseNetDev(strings.NewReader(content))
		if err != nil {
			t.Fatalf("意外解析错误: %v", err)
		}
		if len(stats) != 1 {
			t.Fatalf("期望仅解析 1 个有效网卡，实际为 %d", len(stats))
		}
		if _, ok := stats["eth0"]; !ok {
			t.Fatalf("未解析到 eth0 网卡")
		}
	})
}

// TestNetdevCollector_Filter 验证网卡白名单与黑名单过滤逻辑
func TestNetdevCollector_Filter(t *testing.T) {
	t.Run("默认排除 lo 网卡", func(t *testing.T) {
		col := NewNetdevCollectorWithProc("/proc", nil, regexp.MustCompile("^(lo)$")).(*netdevCollector)
		if col.shouldInclude("lo") {
			t.Errorf("默认配置应排除 lo 网卡")
		}
		if !col.shouldInclude("eth0") {
			t.Errorf("默认配置应包含 eth0 网卡")
		}
		if !col.shouldInclude("br0") {
			t.Errorf("默认配置应包含 br0 网卡")
		}
	})

	t.Run("自定义包含正则", func(t *testing.T) {
		inc := regexp.MustCompile(`^eth[0-9]+$`)
		col := NewNetdevCollectorWithProc("/proc", inc, nil).(*netdevCollector)
		if !col.shouldInclude("eth0") {
			t.Errorf("应包含 eth0")
		}
		if !col.shouldInclude("eth1") {
			t.Errorf("应包含 eth1")
		}
		if col.shouldInclude("br0") {
			t.Errorf("应排除 br0")
		}
		if col.shouldInclude("lo") {
			t.Errorf("应排除 lo")
		}
	})

	t.Run("自定义排除优先于包含", func(t *testing.T) {
		inc := regexp.MustCompile(`^eth[0-9]+$`)
		exc := regexp.MustCompile(`^eth1$`)
		col := NewNetdevCollectorWithProc("/proc", inc, exc).(*netdevCollector)
		if !col.shouldInclude("eth0") {
			t.Errorf("应包含 eth0")
		}
		if col.shouldInclude("eth1") {
			t.Errorf("排除正则应生效排除 eth1")
		}
		if col.shouldInclude("br0") {
			t.Errorf("包含正则应生效排除 br0")
		}
	})
}

// TestNetdevCollector_Update 验证 Update 指标抓取与 Prometheus Metric 输出
func TestNetdevCollector_Update(t *testing.T) {
	tempDir := t.TempDir()
	netDir := filepath.Join(tempDir, "net")
	if err := os.MkdirAll(netDir, 0755); err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}

	devFile := filepath.Join(netDir, "dev")
	if err := os.WriteFile(devFile, []byte(sampleNetDevData), 0644); err != nil {
		t.Fatalf("写入 mock dev 文件失败: %v", err)
	}

	// 默认排除 lo 网卡
	col := NewNetdevCollectorWithProc(tempDir, nil, regexp.MustCompile("^(lo)$"))

	ch := make(chan prometheus.Metric, 50)
	err := col.Update(context.Background(), ch)
	close(ch)

	if err != nil {
		t.Fatalf("Update 抓取失败: %v", err)
	}

	type metricVal struct {
		value      float64
		metricType dto.MetricType
		device     string
	}

	collected := make(map[string][]metricVal)
	for m := range ch {
		dtoMetric := &dto.Metric{}
		if err := m.Write(dtoMetric); err != nil {
			t.Fatalf("序列化 metric 失败: %v", err)
		}

		descStr := m.Desc().String()
		var metricName string
		switch {
		case strings.Contains(descStr, "router_network_receive_bytes_total"):
			metricName = "router_network_receive_bytes_total"
		case strings.Contains(descStr, "router_network_transmit_bytes_total"):
			metricName = "router_network_transmit_bytes_total"
		case strings.Contains(descStr, "router_network_receive_packets_total"):
			metricName = "router_network_receive_packets_total"
		case strings.Contains(descStr, "router_network_transmit_packets_total"):
			metricName = "router_network_transmit_packets_total"
		case strings.Contains(descStr, "router_network_receive_errors_total"):
			metricName = "router_network_receive_errors_total"
		case strings.Contains(descStr, "router_network_transmit_errors_total"):
			metricName = "router_network_transmit_errors_total"
		default:
			t.Fatalf("未知指标: %s", descStr)
		}

		var devLabel string
		for _, label := range dtoMetric.GetLabel() {
			if label.GetName() == "device" {
				devLabel = label.GetValue()
			}
		}

		var val float64
		var mType dto.MetricType
		if dtoMetric.GetCounter() != nil {
			val = dtoMetric.GetCounter().GetValue()
			mType = dto.MetricType_COUNTER
		} else if dtoMetric.GetGauge() != nil {
			val = dtoMetric.GetGauge().GetValue()
			mType = dto.MetricType_GAUGE
		}

		collected[metricName] = append(collected[metricName], metricVal{
			value:      val,
			metricType: mType,
			device:     devLabel,
		})
	}

	// 确认排除 lo，共有 eth0, br0, wl0 三个接口，每个接口 6 个指标，共 18 个指标
	expectedMetrics := []string{
		"router_network_receive_bytes_total",
		"router_network_transmit_bytes_total",
		"router_network_receive_packets_total",
		"router_network_transmit_packets_total",
		"router_network_receive_errors_total",
		"router_network_transmit_errors_total",
	}

	for _, name := range expectedMetrics {
		items, ok := collected[name]
		if !ok {
			t.Fatalf("缺失指标: %s", name)
		}
		if len(items) != 3 {
			t.Fatalf("指标 %s 的接口数量不符，期望 3 (eth0, br0, wl0)，实际为 %d", name, len(items))
		}
		for _, item := range items {
			if item.device == "lo" {
				t.Errorf("指标 %s 中不应包含被排除的 lo 网卡", name)
			}
			if item.metricType != dto.MetricType_COUNTER {
				t.Errorf("指标 %s 类型不符，期望 COUNTER，实际为 %v", name, item.metricType)
			}
		}
	}

	// 检查具体接口的数值准确性
	findVal := func(metricName, dev string) float64 {
		for _, item := range collected[metricName] {
			if item.device == dev {
				return item.value
			}
		}
		t.Fatalf("未找到指标 %s 中网卡 %s 的数据", metricName, dev)
		return 0
	}

	if v := findVal("router_network_receive_bytes_total", "eth0"); v != 12345678 {
		t.Errorf("eth0 rx_bytes 不符，期望 12345678，实际为 %v", v)
	}
	if v := findVal("router_network_transmit_bytes_total", "eth0"); v != 87654321 {
		t.Errorf("eth0 tx_bytes 不符，期望 87654321，实际为 %v", v)
	}
	if v := findVal("router_network_receive_packets_total", "eth0"); v != 12345 {
		t.Errorf("eth0 rx_packets 不符，期望 12345，实际为 %v", v)
	}
	if v := findVal("router_network_transmit_packets_total", "eth0"); v != 54321 {
		t.Errorf("eth0 tx_packets 不符，期望 54321，实际为 %v", v)
	}
	if v := findVal("router_network_receive_errors_total", "eth0"); v != 1 {
		t.Errorf("eth0 rx_errors 不符，期望 1，实际为 %v", v)
	}
	if v := findVal("router_network_transmit_errors_total", "eth0"); v != 2 {
		t.Errorf("eth0 tx_errors 不符，期望 2，实际为 %v", v)
	}
}

// TestNetdevCollector_Update_ContextCanceled 验证 Context 超时或取消时及时退出
func TestNetdevCollector_Update_ContextCanceled(t *testing.T) {
	tempDir := t.TempDir()
	col := NewNetdevCollectorWithProc(tempDir, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	ch := make(chan prometheus.Metric, 10)
	err := col.Update(ctx, ch)
	if err == nil {
		t.Fatalf("期望已取消的 Context 返回错误，但得到 nil")
	}
}

// TestNetdevCollector_Update_FileErrors 验证缺失文件时返回明确错误
func TestNetdevCollector_Update_FileErrors(t *testing.T) {
	t.Run("不存在的 proc 路径", func(t *testing.T) {
		col := NewNetdevCollectorWithProc("/path/that/does/not/exist", nil, nil)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err == nil {
			t.Fatalf("期望不存在路径返回错误，但得到 nil")
		}
	})

	t.Run("缺失 net/dev 文件", func(t *testing.T) {
		tempDir := t.TempDir()
		col := NewNetdevCollectorWithProc(tempDir, nil, nil)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err == nil {
			t.Fatalf("期望缺失 dev 文件时返回错误，但得到 nil")
		}
	})

	t.Run("net/dev 文件内容损坏", func(t *testing.T) {
		tempDir := t.TempDir()
		netDir := filepath.Join(tempDir, "net")
		_ = os.MkdirAll(netDir, 0755)
		_ = os.WriteFile(filepath.Join(netDir, "dev"), []byte("invalid data without colon\n"), 0644)
		col := NewNetdevCollectorWithProc(tempDir, nil, nil)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err == nil {
			t.Fatalf("期望损坏文件内容返回错误，但得到 nil")
		}
	})
}

// TestNetdevCollector_Registration 验证注册中心及工厂函数
func TestNetdevCollector_Registration(t *testing.T) {
	RegisterNetdevCollector()

	avail := AvailableCollectors()
	if enabled, ok := avail["netdev"]; !ok || !enabled {
		t.Fatalf("期望 netdev 采集器已注册且默认启用，实际 enabled: %v, ok: %v", enabled, ok)
	}

	factoriesMu.RLock()
	factory, ok := factories["netdev"]
	factoriesMu.RUnlock()

	if !ok {
		t.Fatalf("未在 factories 表中找到 netdev 工厂函数")
	}

	col, err := factory()
	if err != nil {
		t.Fatalf("调用 netdev 工厂函数失败: %v", err)
	}
	if col == nil || col.Name() != "netdev" {
		t.Fatalf("创建的采集器实例异常: %v", col)
	}
}
