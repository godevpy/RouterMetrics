package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metrics/pkg/nvram"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestParsePhyTempsense 验证 wl phy_tempsense 输出文本的多样化解析
func TestParsePhyTempsense(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		expected    float64
		expectError bool
	}{
		{
			name:        "标准十六进制格式 55 (0x37)",
			output:      "55 (0x37)",
			expected:    55.0,
			expectError: false,
		},
		{
			name:        "纯整数输出 55",
			output:      "55",
			expected:    55.0,
			expectError: false,
		},
		{
			name:        "含换行与首尾空白 \n  50 (0x32)  \n",
			output:      "\n  50 (0x32)  \n",
			expected:    50.0,
			expectError: false,
		},
		{
			name:        "浮点数格式 55.5 (0x37)",
			output:      "55.5 (0x37)",
			expected:    55.5,
			expectError: false,
		},
		{
			name:        "纯浮点数输出 52.8",
			output:      "52.8",
			expected:    52.8,
			expectError: false,
		},
		{
			name:        "带前缀文本 Temperature: 61 (0x3d)",
			output:      "Temperature: 61 (0x3d)",
			expected:    61.0,
			expectError: false,
		},
		{
			name:        "零度输出 0 (0x0)",
			output:      "0 (0x0)",
			expected:    0.0,
			expectError: false,
		},
		{
			name:        "负数温度 -5 (0xfb)",
			output:      "-5 (0xfb)",
			expected:    -5.0,
			expectError: false,
		},
		{
			name:        "空输出报错",
			output:      "",
			expected:    0,
			expectError: true,
		},
		{
			name:        "纯空白字符报错",
			output:      "   \t\n  ",
			expected:    0,
			expectError: true,
		},
		{
			name:        "无有效数字输出报错",
			output:      "wl driver not found",
			expected:    0,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val, err := parsePhyTempsense(tc.output)
			if tc.expectError {
				if err == nil {
					t.Fatalf("期望返回错误，但返回了 nil (结果: %v)", val)
				}
				return
			}

			if err != nil {
				t.Fatalf("意外发生错误: %v", err)
			}
			if val != tc.expected {
				t.Errorf("解析结果不符，期望: %v, 实际: %v", tc.expected, val)
			}
		})
	}
}

// TestThermalCollector_CPUTemperature 验证 CPU sysfs 目录扫描与温度解析
func TestThermalCollector_CPUTemperature(t *testing.T) {
	sysDir := t.TempDir()

	// 构造 thermal_zone0: 55000 毫摄氏度, type=cpu-thermal
	zone0 := filepath.Join(sysDir, "class", "thermal", "thermal_zone0")
	if err := os.MkdirAll(zone0, 0755); err != nil {
		t.Fatalf("创建 zone0 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zone0, "temp"), []byte("55000\n"), 0644); err != nil {
		t.Fatalf("写入 zone0 temp 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zone0, "type"), []byte("cpu-thermal\n"), 0644); err != nil {
		t.Fatalf("写入 zone0 type 失败: %v", err)
	}

	// 构造 thermal_zone1: 62500 毫摄氏度, type=soc-thermal
	zone1 := filepath.Join(sysDir, "class", "thermal", "thermal_zone1")
	if err := os.MkdirAll(zone1, 0755); err != nil {
		t.Fatalf("创建 zone1 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zone1, "temp"), []byte("62500\n"), 0644); err != nil {
		t.Fatalf("写入 zone1 temp 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zone1, "type"), []byte("soc-thermal\n"), 0644); err != nil {
		t.Fatalf("写入 zone1 type 失败: %v", err)
	}

	// 构造 thermal_zone2: 48000 毫摄氏度, 无 type 文件 (应回退为 unknown)
	zone2 := filepath.Join(sysDir, "class", "thermal", "thermal_zone2")
	if err := os.MkdirAll(zone2, 0755); err != nil {
		t.Fatalf("创建 zone2 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zone2, "temp"), []byte("48000\n"), 0644); err != nil {
		t.Fatalf("写入 zone2 temp 失败: %v", err)
	}

	// 构造 thermal_zone3: 非法 temp 内容，应容错跳过
	zone3 := filepath.Join(sysDir, "class", "thermal", "thermal_zone3")
	if err := os.MkdirAll(zone3, 0755); err != nil {
		t.Fatalf("创建 zone3 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zone3, "temp"), []byte("invalid_temp\n"), 0644); err != nil {
		t.Fatalf("写入 zone3 temp 失败: %v", err)
	}

	col := NewThermalCollectorWithConfig(sysDir, nvram.NewMockClient(nil), nil)

	ch := make(chan prometheus.Metric, 20)
	ctx := context.Background()

	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("col.Update 发生意外错误: %v", err)
	}
	close(ch)

	type metricKey struct {
		zone     string
		zoneType string
	}
	cpuMetrics := make(map[metricKey]float64)

	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		descStr := m.Desc().String()
		if strings.Contains(descStr, "router_cpu_temperature_celsius") {
			var zone, ztype string
			for _, lbl := range dtoMetric.GetLabel() {
				if lbl.GetName() == "zone" {
					zone = lbl.GetValue()
				} else if lbl.GetName() == "type" {
					ztype = lbl.GetValue()
				}
			}
			cpuMetrics[metricKey{zone: zone, zoneType: ztype}] = dtoMetric.GetGauge().GetValue()
		}
	}

	if len(cpuMetrics) != 3 {
		t.Fatalf("期望采集到 3 个有效 CPU 温度指标，实际采集到 %d: %+v", len(cpuMetrics), cpuMetrics)
	}

	expected := map[metricKey]float64{
		{zone: "0", zoneType: "cpu-thermal"}: 55.0,
		{zone: "1", zoneType: "soc-thermal"}: 62.5,
		{zone: "2", zoneType: "unknown"}:     48.0,
	}

	for k, expVal := range expected {
		val, ok := cpuMetrics[k]
		if !ok {
			t.Errorf("缺少期望 CPU 指标: %+v", k)
			continue
		}
		if val != expVal {
			t.Errorf("CPU 温度指标 %+v 数值不符，期望: %v, 实际: %v", k, expVal, val)
		}
	}
}

// TestThermalCollector_WifiTemperature 验证 Wi-Fi 温度基于 NVRAM 网卡发现及 wl 命令采集
func TestThermalCollector_WifiTemperature(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl1_ifname": "eth7",
		"wl2_ifname": "eth8",
	}
	client := nvram.NewMockClient(mockNvram)

	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if name != "wl" {
			return nil, fmt.Errorf("unexpected command: %s", name)
		}
		if len(args) != 3 || args[0] != "-i" || args[2] != "phy_tempsense" {
			return nil, fmt.Errorf("unexpected args: %v", args)
		}
		switch args[1] {
		case "eth6":
			return []byte("51 (0x33)\n"), nil
		case "eth7":
			return []byte("58 (0x3a)\n"), nil
		case "eth8":
			return []byte("63 (0x3f)\n"), nil
		default:
			return nil, fmt.Errorf("unknown interface: %s", args[1])
		}
	}

	emptySys := t.TempDir()
	col := NewThermalCollectorWithConfig(emptySys, client, mockRunner)

	ch := make(chan prometheus.Metric, 20)
	ctx := context.Background()

	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("col.Update 发生意外错误: %v", err)
	}
	close(ch)

	type wifiKey struct {
		iface string
		band  string
	}
	wifiMetrics := make(map[wifiKey]float64)

	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		descStr := m.Desc().String()
		if strings.Contains(descStr, "router_wifi_temperature_celsius") {
			var iface, band string
			for _, lbl := range dtoMetric.GetLabel() {
				if lbl.GetName() == "interface" {
					iface = lbl.GetValue()
				} else if lbl.GetName() == "band" {
					band = lbl.GetValue()
				}
			}
			wifiMetrics[wifiKey{iface: iface, band: band}] = dtoMetric.GetGauge().GetValue()
		}
	}

	if len(wifiMetrics) != 3 {
		t.Fatalf("期望采集到 3 个 Wi-Fi 频段温度指标，实际为 %d: %+v", len(wifiMetrics), wifiMetrics)
	}

	expected := map[wifiKey]float64{
		{iface: "eth6", band: "2.4GHz"}:       51.0,
		{iface: "eth7", band: "5GHz"}:         58.0,
		{iface: "eth8", band: "5GHz-2/6GHz"}: 63.0,
	}

	for k, expVal := range expected {
		val, ok := wifiMetrics[k]
		if !ok {
			t.Errorf("缺少期望 Wi-Fi 指标: %+v", k)
			continue
		}
		if val != expVal {
			t.Errorf("Wi-Fi 温度指标 %+v 数值不符，期望: %v, 实际: %v", k, expVal, val)
		}
	}
}

// TestThermalCollector_WifiFaultTolerance 验证单个网卡执行超时或输出异常时的故障隔离能力
func TestThermalCollector_WifiFaultTolerance(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "eth6",
		"wl1_ifname": "eth7",
		"wl2_ifname": "eth8",
	}
	client := nvram.NewMockClient(mockNvram)

	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		switch args[1] {
		case "eth6":
			return []byte("52 (0x34)\n"), nil
		case "eth7":
			// 模拟底层驱动挂起超时
			return nil, fmt.Errorf("command wl [-i eth7 phy_tempsense] timed out after 800ms")
		case "eth8":
			// 模拟输出不可解析内容
			return []byte("wl: driver adapter not found\n"), nil
		default:
			return nil, fmt.Errorf("unknown interface: %s", args[1])
		}
	}

	emptySys := t.TempDir()
	col := NewThermalCollectorWithConfig(emptySys, client, mockRunner)

	ch := make(chan prometheus.Metric, 20)
	ctx := context.Background()

	// 即使单个网卡超时或解析失败，也绝不能抛出 panic 或中断整个收集流程
	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("部分网卡失败时 Update 不应返回致命错误，实际为: %v", err)
	}
	close(ch)

	var emittedCount int
	var eth6Val float64
	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		descStr := m.Desc().String()
		if strings.Contains(descStr, "router_wifi_temperature_celsius") {
			emittedCount++
			for _, lbl := range dtoMetric.GetLabel() {
				if lbl.GetName() == "interface" && lbl.GetValue() == "eth6" {
					eth6Val = dtoMetric.GetGauge().GetValue()
				}
			}
		}
	}

	if emittedCount != 1 {
		t.Fatalf("期望仅正常网卡发出 1 个指标，实际发出: %d", emittedCount)
	}
	if eth6Val != 52.0 {
		t.Errorf("eth6 指标数值不符，期望 52.0，实际: %v", eth6Val)
	}
}

// TestThermalCollector_PartialNVRAM 验证某些频段未配置时的自适应跳过
func TestThermalCollector_PartialNVRAM(t *testing.T) {
	mockNvram := map[string]string{
		"wl0_ifname": "wl0",
		"wl1_ifname": "",
		// wl2_ifname 未设置
	}
	client := nvram.NewMockClient(mockNvram)

	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if args[1] == "wl0" {
			return []byte("49 (0x31)\n"), nil
		}
		return nil, fmt.Errorf("unexpected interface call: %s", args[1])
	}

	emptySys := t.TempDir()
	col := NewThermalCollectorWithConfig(emptySys, client, mockRunner)

	ch := make(chan prometheus.Metric, 20)
	ctx := context.Background()

	if err := col.Update(ctx, ch); err != nil {
		t.Fatalf("Update 发生意外错误: %v", err)
	}
	close(ch)

	var count int
	for m := range ch {
		if strings.Contains(m.Desc().String(), "router_wifi_temperature_celsius") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("期望仅采集到 1 个频段温度，实际为: %d", count)
	}
}

// TestThermalCollector_ContextCancellation 验证 Context 取消时立即中断
func TestThermalCollector_ContextCancellation(t *testing.T) {
	emptySys := t.TempDir()
	col := NewThermalCollectorWithConfig(emptySys, nvram.NewMockClient(nil), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan prometheus.Metric, 10)
	err := col.Update(ctx, ch)
	if err != context.Canceled {
		t.Fatalf("期望返回 context.Canceled，实际为: %v", err)
	}
}

// TestThermalCollector_Registration 验证采集器注册及工厂函数
func TestThermalCollector_Registration(t *testing.T) {
	RegisterThermalCollector()

	available := AvailableCollectors()
	enabled, ok := available["thermal"]
	if !ok {
		t.Fatalf("AvailableCollectors 中未找到 thermal 采集器")
	}
	if !enabled {
		t.Errorf("thermal 采集器默认状态应为 true")
	}

	factoriesMu.RLock()
	factory, ok := factories["thermal"]
	factoriesMu.RUnlock()

	if !ok {
		t.Fatalf("未在 factories 表中找到 thermal 工厂函数")
	}

	col, err := factory()
	if err != nil {
		t.Fatalf("调用 thermal 工厂函数失败: %v", err)
	}
	if col == nil || col.Name() != "thermal" {
		t.Fatalf("创建的采集器实例异常: %v", col)
	}
}

// errorNVRAMClient 用于模拟 GetAll 失败的 NVRAM 客户端
type errorNVRAMClient struct{}

func (e *errorNVRAMClient) Get(ctx context.Context, key string) (string, error) {
	return "", fmt.Errorf("nvram get error")
}

func (e *errorNVRAMClient) GetAll(ctx context.Context, keys []string) (map[string]string, error) {
	return nil, fmt.Errorf("nvram getall error")
}

// TestThermalCollector_EdgeCases 验证边界条件与异常分支处理
func TestThermalCollector_EdgeCases(t *testing.T) {
	t.Run("默认配置回退", func(t *testing.T) {
		col := NewThermalCollectorWithConfig("", nil, nil)
		if col == nil {
			t.Fatal("期望返回非空采集器")
		}
		tc, ok := col.(*thermalCollector)
		if !ok {
			t.Fatalf("期望类型为 *thermalCollector")
		}
		if tc.sysPath != "/sys" {
			t.Errorf("sysPath 期望 /sys, 实际: %s", tc.sysPath)
		}
		if tc.nvramClient == nil {
			t.Errorf("nvramClient 不应为空")
		}
		if tc.cmdRunner == nil {
			t.Errorf("cmdRunner 不应为空")
		}
	})

	t.Run("缺失 temp 文件跳过处理", func(t *testing.T) {
		sysDir := t.TempDir()
		zone0 := filepath.Join(sysDir, "class", "thermal", "thermal_zone0")
		_ = os.MkdirAll(zone0, 0755)
		// 不创建 temp 文件，仅创建 type 文件
		_ = os.WriteFile(filepath.Join(zone0, "type"), []byte("cpu-thermal\n"), 0644)

		col := NewThermalCollectorWithConfig(sysDir, nvram.NewMockClient(nil), nil)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err != nil {
			t.Fatalf("缺失 temp 文件时不应返回致命错误: %v", err)
		}
		close(ch)
		if len(ch) != 0 {
			t.Errorf("期望无指标推入，实际推入 %d 个", len(ch))
		}
	})

	t.Run("NVRAM GetAll 返回错误时优雅降级", func(t *testing.T) {
		emptySys := t.TempDir()
		col := NewThermalCollectorWithConfig(emptySys, &errorNVRAMClient{}, nil)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(context.Background(), ch)
		if err != nil {
			t.Fatalf("NVRAM 异常时应平滑忽略，实际返回错误: %v", err)
		}
	})

	t.Run("nil NVRAMClient 处理", func(t *testing.T) {
		tc := &thermalCollector{
			sysPath:     t.TempDir(),
			nvramClient: nil,
			cmdRunner:   nil,
		}
		ch := make(chan prometheus.Metric, 10)
		err := tc.collectWifiTemps(context.Background(), ch)
		if err != nil {
			t.Fatalf("nil nvramClient 时应安全返回 nil: %v", err)
		}
	})

	t.Run("Wi-Fi 循环中检测 Context 取消", func(t *testing.T) {
		mockNvram := map[string]string{
			"wl0_ifname": "eth6",
			"wl1_ifname": "eth7",
		}
		client := nvram.NewMockClient(mockNvram)

		ctx, cancel := context.WithCancel(context.Background())
		mockRunner := func(c context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
			cancel() // 在首次调用时取消 Context
			return []byte("50 (0x32)"), nil
		}

		col := NewThermalCollectorWithConfig(t.TempDir(), client, mockRunner)
		ch := make(chan prometheus.Metric, 10)
		err := col.Update(ctx, ch)
		if err != context.Canceled {
			t.Fatalf("期望返回 context.Canceled，实际为: %v", err)
		}
	})
}

