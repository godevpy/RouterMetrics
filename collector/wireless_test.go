package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"metrics/pkg/nvram"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestParseAssoclist 验证 wl assoclist 输出的 MAC 地址提取与统计解析
func TestParseAssoclist(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected []string
	}{
		{
			name: "标准带 assoclist 前缀输出",
			output: `assoclist 00:11:22:33:44:55
assoclist AA:BB:CC:DD:EE:FF
assoclist 12:34:56:78:9a:bc
`,
			expected: []string{
				"00:11:22:33:44:55",
				"aa:bb:cc:dd:ee:ff",
				"12:34:56:78:9a:bc",
			},
		},
		{
			name: "纯 MAC 地址无前缀输出",
			output: `00:11:22:33:44:55
AA:BB:CC:DD:EE:FF
`,
			expected: []string{
				"00:11:22:33:44:55",
				"aa:bb:cc:dd:ee:ff",
			},
		},
		{
			name: "包含多余空格与空白行",
			output: `
   assoclist   00:11:22:33:44:55   

   assoclist aa:bb:cc:dd:ee:ff  
`,
			expected: []string{
				"00:11:22:33:44:55",
				"aa:bb:cc:dd:ee:ff",
			},
		},
		{
			name:     "空输出（无连接终端）",
			output:   "",
			expected: []string{},
		},
		{
			name:     "纯空白字符输出",
			output:   "   \t\n  \r\n   ",
			expected: []string{},
		},
		{
			name: "混入驱动日志及无效格式行",
			output: `assoclist 00:11:22:33:44:55
wl: driver warning: buffer threshold exceeded
assoclist 00:11:22:33:44
assoclist 00:11:22:33:44:55:66:77
assoclist GG:HH:II:JJ:KK:LL
assoclist AA:BB:CC:DD:EE:FF
`,
			expected: []string{
				"00:11:22:33:44:55",
				"aa:bb:cc:dd:ee:ff",
			},
		},
		{
			name: "重复 MAC 地址自动去重",
			output: `assoclist 00:11:22:33:44:55
assoclist 00:11:22:33:44:55
assoclist AA:BB:CC:DD:EE:FF
assoclist aa:bb:cc:dd:ee:ff
`,
			expected: []string{
				"00:11:22:33:44:55",
				"aa:bb:cc:dd:ee:ff",
			},
		},
		{
			name: "行尾带有附加信息",
			output: `assoclist 00:11:22:33:44:55 [rssi -65]
assoclist AA:BB:CC:DD:EE:FF (idle 12s)
`,
			expected: []string{
				"00:11:22:33:44:55",
				"aa:bb:cc:dd:ee:ff",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAssoclist(tc.output)
			if len(got) != len(tc.expected) {
				t.Fatalf("提取 MAC 数量不匹配，期望 %d (%v)，实际得到 %d (%v)",
					len(tc.expected), tc.expected, len(got), got)
			}
			for i, exp := range tc.expected {
				if got[i] != exp {
					t.Errorf("第 %d 个 MAC 不匹配，期望: %s, 实际: %s", i, exp, got[i])
				}
			}
		})
	}
}

// TestWirelessCollector_MultiBandClientsAndRadio 验证多频段（2.4G/5G-1/5G-2）客户端及射频开关采集
func TestWirelessCollector_MultiBandClientsAndRadio(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl0_radio":  "1",
		"wl0_ssid":   "Home_2.4G",
		"wl1_ifname": "eth7",
		"wl1_radio":  "1",
		"wl1_ssid":   "Home_5G",
		"wl2_ifname": "eth8",
		"wl2_radio":  "0",
		"wl2_ssid":   "Home_6G",
	}
	client := nvram.NewMockClient(mockNvram)

	var wlCalledCount int32
	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		atomic.AddInt32(&wlCalledCount, 1)
		if name != "wl" {
			return nil, fmt.Errorf("unexpected command: %s", name)
		}
		if len(args) != 3 || args[0] != "-i" || args[2] != "assoclist" {
			return nil, fmt.Errorf("unexpected args: %v", args)
		}
		switch args[1] {
		case "eth6":
			return []byte("assoclist 00:11:22:33:44:55\nassoclist aa:bb:cc:dd:ee:ff\n"), nil
		case "eth7":
			return []byte("assoclist 11:22:33:44:55:66\nassoclist 22:33:44:55:66:77\nassoclist 33:44:55:66:77:88\n"), nil
		case "eth8":
			t.Fatalf("禁用射频的网卡 eth8 不应触发 wl assoclist 执行")
			return nil, nil
		default:
			return nil, fmt.Errorf("unknown interface: %s", args[1])
		}
	}

	col := NewWirelessCollectorWithConfig(client, mockRunner)
	ch := make(chan prometheus.Metric, 20)
	ctx := context.Background()

	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("col.Update 返回意外错误: %v", err)
	}
	close(ch)

	type radioKey struct {
		iface string
		band  string
	}
	type clientKey struct {
		iface string
		band  string
		ssid  string
	}

	radioMetrics := make(map[radioKey]float64)
	clientMetrics := make(map[clientKey]float64)

	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		descStr := m.Desc().String()
		if strings.Contains(descStr, "router_wifi_radio_enabled") {
			var iface, band string
			for _, lbl := range dtoMetric.GetLabel() {
				if lbl.GetName() == "interface" {
					iface = lbl.GetValue()
				} else if lbl.GetName() == "band" {
					band = lbl.GetValue()
				}
			}
			radioMetrics[radioKey{iface: iface, band: band}] = dtoMetric.GetGauge().GetValue()
		} else if strings.Contains(descStr, "router_wifi_connected_clients") {
			var iface, band, ssid string
			for _, lbl := range dtoMetric.GetLabel() {
				if lbl.GetName() == "interface" {
					iface = lbl.GetValue()
				} else if lbl.GetName() == "band" {
					band = lbl.GetValue()
				} else if lbl.GetName() == "ssid" {
					ssid = lbl.GetValue()
				}
			}
			clientMetrics[clientKey{iface: iface, band: band, ssid: ssid}] = dtoMetric.GetGauge().GetValue()
		}
	}

	// 验证射频状态指标
	expectedRadio := map[radioKey]float64{
		{iface: "eth6", band: "2.4GHz"}:        1.0,
		{iface: "eth7", band: "5GHz-1"}:        1.0,
		{iface: "eth8", band: "5GHz-2 / 6GHz"}: 0.0,
	}
	if len(radioMetrics) != len(expectedRadio) {
		t.Fatalf("期望采集到 %d 个射频状态指标，实际采集到 %d: %+v",
			len(expectedRadio), len(radioMetrics), radioMetrics)
	}
	for k, exp := range expectedRadio {
		if got, ok := radioMetrics[k]; !ok || got != exp {
			t.Errorf("射频状态指标 %+v 期望 %v，实际得到 %v (存在: %v)", k, exp, got, ok)
		}
	}

	// 验证连接客户端数量指标
	expectedClients := map[clientKey]float64{
		{iface: "eth6", band: "2.4GHz", ssid: "Home_2.4G"}:      2.0,
		{iface: "eth7", band: "5GHz-1", ssid: "Home_5G"}:        3.0,
		{iface: "eth8", band: "5GHz-2 / 6GHz", ssid: "Home_6G"}: 0.0,
	}
	if len(clientMetrics) != len(expectedClients) {
		t.Fatalf("期望采集到 %d 个客户端连接指标，实际采集到 %d: %+v",
			len(expectedClients), len(clientMetrics), clientMetrics)
	}
	for k, exp := range expectedClients {
		if got, ok := clientMetrics[k]; !ok || got != exp {
			t.Errorf("客户端连接指标 %+v 期望 %v，实际得到 %v (存在: %v)", k, exp, got, ok)
		}
	}

	// 确保 wl assoclist 仅针对启用的网卡（eth6, eth7）调用了 2 次
	if wlCalledCount != 2 {
		t.Errorf("期望 wl 命令被调用 2 次，实际调用 %d 次", wlCalledCount)
	}
}

// TestWirelessCollector_RadioDisabled 验证射频全部关闭时，不调用 wl 命令且报告客户端为 0
func TestWirelessCollector_RadioDisabled(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl0_radio":  "0",
		"wl0_ssid":   "Home_2.4G",
		"wl1_ifname": "eth7",
		"wl1_radio":  "0",
		"wl1_ssid":   "Home_5G",
	}
	client := nvram.NewMockClient(mockNvram)

	var wlCalled bool
	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		wlCalled = true
		return nil, errors.New("should not be called")
	}

	col := NewWirelessCollectorWithConfig(client, mockRunner)
	ch := make(chan prometheus.Metric, 10)
	ctx := context.Background()

	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("col.Update 发生错误: %v", err)
	}
	close(ch)

	if wlCalled {
		t.Fatal("射频关闭时，不应调用任何外部 wl 命令")
	}

	metricCount := 0
	for m := range ch {
		metricCount++
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		if dtoMetric.GetGauge().GetValue() != 0.0 {
			t.Errorf("射频关闭时指标值期望为 0，实际为: %v", dtoMetric.GetGauge().GetValue())
		}
	}

	// 2个接口各产生 1个 radio_enabled + 1个 connected_clients = 4个指标
	if metricCount != 4 {
		t.Errorf("期望产生 4 个指标，实际产生 %d 个", metricCount)
	}
}

// TestWirelessCollector_PartialNVRAM 验证部分频段未配置时的自适应跳过
func TestWirelessCollector_PartialNVRAM(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl0_radio":  "1",
		"wl0_ssid":   "SingleBand_2.4G",
		// wl1, wl2 未配置或为空
		"wl1_ifname": "",
		"wl2_ifname": "   ",
	}
	client := nvram.NewMockClient(mockNvram)

	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		return []byte("assoclist 00:11:22:33:44:55\n"), nil
	}

	col := NewWirelessCollectorWithConfig(client, mockRunner)
	ch := make(chan prometheus.Metric, 10)
	ctx := context.Background()

	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("col.Update 发生错误: %v", err)
	}
	close(ch)

	metricCount := 0
	for m := range ch {
		metricCount++
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		for _, lbl := range dtoMetric.GetLabel() {
			if lbl.GetName() == "interface" && lbl.GetValue() != "eth6" {
				t.Errorf("不应采集未配置的网卡指标: %s", lbl.GetValue())
			}
		}
	}

	if metricCount != 2 {
		t.Errorf("期望仅产生 2 个指标（eth6 的 radio 与 clients），实际为 %d", metricCount)
	}
}

// TestWirelessCollector_CommandFailureTolerance 验证单个接口 wl 命令执行失败时平滑降级
func TestWirelessCollector_CommandFailureTolerance(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl0_radio":  "1",
		"wl0_ssid":   "Home_2.4G",
		"wl1_ifname": "eth7",
		"wl1_radio":  "1",
		"wl1_ssid":   "Home_5G",
	}
	client := nvram.NewMockClient(mockNvram)

	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if args[1] == "eth7" {
			return nil, errors.New("wl: exit status 1: Broadcom driver error")
		}
		return []byte("assoclist 00:11:22:33:44:55\n"), nil
	}

	col := NewWirelessCollectorWithConfig(client, mockRunner)
	ch := make(chan prometheus.Metric, 10)
	ctx := context.Background()

	// Update 应平滑成功，不因单个接口失败导致整个采集失败
	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("单个网卡异常时不应导致整体 Update 失败: %v", err)
	}
	close(ch)

	var eth6ClientsFound, eth7RadioFound bool
	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		descStr := m.Desc().String()
		var iface string
		for _, lbl := range dtoMetric.GetLabel() {
			if lbl.GetName() == "interface" {
				iface = lbl.GetValue()
			}
		}
		if iface == "eth6" && strings.Contains(descStr, "router_wifi_connected_clients") {
			eth6ClientsFound = true
			if dtoMetric.GetGauge().GetValue() != 1.0 {
				t.Errorf("eth6 客户端数量期望 1.0，实际: %v", dtoMetric.GetGauge().GetValue())
			}
		}
		if iface == "eth7" && strings.Contains(descStr, "router_wifi_radio_enabled") {
			eth7RadioFound = true
			if dtoMetric.GetGauge().GetValue() != 1.0 {
				t.Errorf("eth7 射频状态期望 1.0，实际: %v", dtoMetric.GetGauge().GetValue())
			}
		}
	}

	if !eth6ClientsFound {
		t.Error("未找到正常接口 eth6 的客户端指标")
	}
	if !eth7RadioFound {
		t.Error("未找到异常接口 eth7 的射频状态指标（应由 NVRAM 正常提供）")
	}
}

// TestWirelessCollector_CommandTimeout 验证 wl 命令超时时的平滑容错
func TestWirelessCollector_CommandTimeout(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl0_radio":  "1",
		"wl0_ssid":   "Home_2.4G",
	}
	client := nvram.NewMockClient(mockNvram)

	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}

	col := NewWirelessCollectorWithConfig(client, mockRunner)
	ch := make(chan prometheus.Metric, 10)
	ctx := context.Background()

	// 命令级超时不应导致全局 Update 返回错误
	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("外部命令超时时 Update 应平滑容错返回 nil，实际返回: %v", err)
	}
	close(ch)
}

// TestWirelessCollector_ContextCancelled 验证抓取 Context 取消时的及时退出
func TestWirelessCollector_ContextCancelled(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl0_radio":  "1",
	}
	client := nvram.NewMockClient(mockNvram)

	col := NewWirelessCollectorWithConfig(client, nil)
	ch := make(chan prometheus.Metric, 10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消 context

	err := col.Update(ctx, ch)
	if err == nil {
		t.Fatal("Context 已取消时 Update 应返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("期望返回 context.Canceled，实际得到: %v", err)
	}
}

// TestWirelessCollector_NVRAMFailure 验证 NVRAM 读取失败或 client 为空时的安全降级
func TestWirelessCollector_NVRAMFailure(t *testing.T) {
	t.Run("NVRAM GetAll 返回错误时优雅降级", func(t *testing.T) {
		col := NewWirelessCollectorWithConfig(&errorNVRAMClient{}, nil)
		ch := make(chan prometheus.Metric, 10)
		ctx := context.Background()

		err := col.Update(ctx, ch)
		if err != nil {
			t.Fatalf("NVRAM 异常时应平滑忽略，实际返回错误: %v", err)
		}
		close(ch)
	})

	t.Run("nil NVRAMClient 处理", func(t *testing.T) {
		col := &wirelessCollector{
			nvramClient: nil,
			cmdRunner:   nil,
		}
		ch := make(chan prometheus.Metric, 10)
		ctx := context.Background()

		err := col.Update(ctx, ch)
		if err != nil {
			t.Fatalf("nil nvramClient 时应平滑返回 nil，实际返回: %v", err)
		}
		close(ch)
	})

	t.Run("NewWirelessCollectorWithConfig 传入 nil 时回退默认实现", func(t *testing.T) {
		col := NewWirelessCollectorWithConfig(nil, nil)
		if col == nil || col.Name() != "wireless" {
			t.Fatalf("回退默认构造的采集器异常: %v", col)
		}
	})
}

// TestWirelessCollector_Registration 验证采集器注册中心及 Merlin 整体聚合集成
func TestWirelessCollector_Registration(t *testing.T) {
	RegisterWirelessCollector()

	// 验证注册表中包含 wireless 采集器且默认启用
	all := AvailableCollectors()
	enabled, exists := all["wireless"]
	if !exists {
		t.Fatal("采集器 wireless 未在注册表中找到")
	}
	if !enabled {
		t.Fatal("采集器 wireless 默认状态应为已启用 (true)")
	}

	// 验证 NewWirelessCollector 构造函数
	col, err := NewWirelessCollector()
	if err != nil {
		t.Fatalf("NewWirelessCollector 返回错误: %v", err)
	}
	if col.Name() != "wireless" {
		t.Fatalf("采集器 Name() 期望为 'wireless'，实际为: %s", col.Name())
	}

	// 验证 MerlinCollector 能够正常调度该采集器
	merlin, err := NewMerlinCollector(1*time.Second, map[string]bool{"wireless": true})
	if err != nil {
		t.Fatalf("NewMerlinCollector 初始化失败: %v", err)
	}
	if _, ok := merlin.collectors["wireless"]; !ok {
		t.Fatal("MerlinCollector active 列表中未找到 wireless 采集器")
	}
}
