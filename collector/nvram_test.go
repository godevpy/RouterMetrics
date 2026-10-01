package collector

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"metrics/pkg/nvram"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// nvramFailingClient 用于测试 NVRAM 客户端查询失败时的错误传播
type nvramFailingClient struct{}

func (e *nvramFailingClient) Get(ctx context.Context, key string) (string, error) {
	return "", errors.New("nvram client get failure")
}

func (e *nvramFailingClient) GetAll(ctx context.Context, keys []string) (map[string]string, error) {
	return nil, errors.New("nvram client getall failure")
}

// blockingNVRAMClient 用于测试 Context 超时或取消时的即时中断
type blockingNVRAMClient struct{}

func (b *blockingNVRAMClient) Get(ctx context.Context, key string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (b *blockingNVRAMClient) GetAll(ctx context.Context, keys []string) (map[string]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// collectMetrics 是测试辅助函数，用于执行 Update 并提取收集到的所有 Metric
func collectNVRAMMetrics(t *testing.T, col Collector, ctx context.Context) (map[string][]metricData, error) {
	t.Helper()
	ch := make(chan prometheus.Metric, 50)
	err := col.Update(ctx, ch)
	close(ch)

	res := make(map[string][]metricData)
	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("序列化指标失败: %v", err)
		}
		descStr := m.Desc().String()
		labels := make(map[string]string)
		for _, lbl := range dtoMetric.GetLabel() {
			labels[lbl.GetName()] = lbl.GetValue()
		}

		var val float64
		if dtoMetric.Gauge != nil {
			val = dtoMetric.GetGauge().GetValue()
		} else if dtoMetric.Counter != nil {
			val = dtoMetric.GetCounter().GetValue()
		}

		name := extractMetricNameFromDesc(descStr)
		res[name] = append(res[name], metricData{
			labels: labels,
			value:  val,
		})
	}
	return res, err
}

type metricData struct {
	labels map[string]string
	value  float64
}

func extractMetricNameFromDesc(descStr string) string {
	// 格式例如: Desc{fqName: "router_info", ...}
	idx := strings.Index(descStr, `fqName: "`)
	if idx == -1 {
		return ""
	}
	sub := descStr[idx+len(`fqName: "`):]
	endIdx := strings.Index(sub, `"`)
	if endIdx == -1 {
		return ""
	}
	return sub[:endIdx]
}

// TestNVRAMCollector_FullMapping 验证全量 NVRAM 变量完整映射与指标输出
func TestNVRAMCollector_FullMapping(t *testing.T) {
	mockData := map[string]string{
		"productid":     "RT-AX86U",
		"buildno":       "3004.388.4",
		"extendno":      "0",
		"sw_mode":       "1",
		"lan_ipaddr":    "192.168.50.1",
		"lan_netmask":   "255.255.255.0",
		"wan0_ipaddr":   "100.64.1.2",
		"wan0_gateway":  "100.64.1.1",
		"wan0_dns":      "8.8.8.8 1.1.1.1",
		"wan0_state_t":  "2",
		"wan0_proto":    "dhcp",
		"wan0_uptime":   "86400",
		"link_internet": "2",
		"ntp_ready":     "1",
	}
	client := nvram.NewMockClient(mockData)
	col := NewNVRAMCollectorWithConfig(client, "arm64")

	metrics, err := collectNVRAMMetrics(t, col, context.Background())
	if err != nil {
		t.Fatalf("col.Update 发生意外错误: %v", err)
	}

	// 1. router_info
	infoList, ok := metrics["router_info"]
	if !ok || len(infoList) != 1 {
		t.Fatalf("未找到或存在多条 router_info 指标: %v", infoList)
	}
	if infoList[0].value != 1.0 {
		t.Errorf("router_info 值应恒为 1.0, 实际为: %f", infoList[0].value)
	}
	if got := infoList[0].labels["product_id"]; got != "RT-AX86U" {
		t.Errorf("product_id 标签不匹配, 期望: RT-AX86U, 实际: %s", got)
	}
	if got := infoList[0].labels["firmware_version"]; got != "3004.388.4_0" {
		t.Errorf("firmware_version 标签不匹配, 期望: 3004.388.4_0, 实际: %s", got)
	}
	if got := infoList[0].labels["architecture"]; got != "arm64" {
		t.Errorf("architecture 标签不匹配, 期望: arm64, 实际: %s", got)
	}

	// 2. router_network_mode
	modeList, ok := metrics["router_network_mode"]
	if !ok || len(modeList) != 1 {
		t.Fatalf("未找到或存在多条 router_network_mode 指标: %v", modeList)
	}
	if modeList[0].value != 1.0 {
		t.Errorf("router_network_mode 值应恒为 1.0, 实际为: %f", modeList[0].value)
	}
	if got := modeList[0].labels["mode"]; got != "router" {
		t.Errorf("router_network_mode mode 标签不匹配, 期望: router, 实际: %s", got)
	}

	// 3. router_gateway_info
	gwList, ok := metrics["router_gateway_info"]
	if !ok || len(gwList) != 1 {
		t.Fatalf("未找到或存在多条 router_gateway_info 指标: %v", gwList)
	}
	if gwList[0].value != 1.0 {
		t.Errorf("router_gateway_info 值应恒为 1.0, 实际为: %f", gwList[0].value)
	}
	gwLabels := gwList[0].labels
	if gwLabels["lan_ip"] != "192.168.50.1" || gwLabels["lan_netmask"] != "255.255.255.0" ||
		gwLabels["wan_ip"] != "100.64.1.2" || gwLabels["wan_gateway"] != "100.64.1.1" ||
		gwLabels["dns"] != "8.8.8.8 1.1.1.1" {
		t.Errorf("router_gateway_info 标签不符合预期: %+v", gwLabels)
	}

	// 4. router_wan_status
	wanStatusList, ok := metrics["router_wan_status"]
	if !ok || len(wanStatusList) != 1 {
		t.Fatalf("未找到或存在多条 router_wan_status 指标: %v", wanStatusList)
	}
	if wanStatusList[0].value != 1.0 {
		t.Errorf("router_wan_status 连接状态应为 1.0, 实际为: %f", wanStatusList[0].value)
	}
	if wanStatusList[0].labels["interface"] != "wan0" ||
		wanStatusList[0].labels["ip"] != "100.64.1.2" ||
		wanStatusList[0].labels["proto"] != "dhcp" {
		t.Errorf("router_wan_status 标签不匹配: %+v", wanStatusList[0].labels)
	}

	// 5. router_wan_internet_status
	internetList, ok := metrics["router_wan_internet_status"]
	if !ok || len(internetList) != 1 {
		t.Fatalf("未找到或存在多条 router_wan_internet_status 指标: %v", internetList)
	}
	if internetList[0].value != 1.0 {
		t.Errorf("router_wan_internet_status 应为 1.0, 实际为: %f", internetList[0].value)
	}

	// 6. router_wan_uptime_seconds
	uptimeList, ok := metrics["router_wan_uptime_seconds"]
	if !ok || len(uptimeList) != 1 {
		t.Fatalf("未找到或存在多条 router_wan_uptime_seconds 指标: %v", uptimeList)
	}
	if uptimeList[0].value != 86400.0 {
		t.Errorf("router_wan_uptime_seconds 应为 86400.0, 实际为: %f", uptimeList[0].value)
	}
	if uptimeList[0].labels["interface"] != "wan0" {
		t.Errorf("router_wan_uptime_seconds interface 标签应为 wan0, 实际为: %s", uptimeList[0].labels["interface"])
	}

	// 7. router_ntp_synced
	ntpList, ok := metrics["router_ntp_synced"]
	if !ok || len(ntpList) != 1 {
		t.Fatalf("未找到或存在多条 router_ntp_synced 指标: %v", ntpList)
	}
	if ntpList[0].value != 1.0 {
		t.Errorf("router_ntp_synced 应为 1.0, 实际为: %f", ntpList[0].value)
	}
}

// TestNVRAMCollector_NetworkModes 验证 sw_mode 工作模式的映射转换
func TestNVRAMCollector_NetworkModes(t *testing.T) {
	tests := []struct {
		swMode   string
		expected string
	}{
		{swMode: "1", expected: "router"},
		{swMode: "2", expected: "repeater"},
		{swMode: "3", expected: "ap"},
		{swMode: "4", expected: "media_bridge"},
		{swMode: "5", expected: "unknown"},
		{swMode: "0", expected: "unknown"},
		{swMode: "", expected: "unknown"},
		{swMode: "invalid", expected: "unknown"},
		{swMode: " 1 ", expected: "router"},
	}

	for _, tc := range tests {
		t.Run("sw_mode_"+tc.swMode, func(t *testing.T) {
			client := nvram.NewMockClient(map[string]string{
				"sw_mode": tc.swMode,
			})
			col := NewNVRAMCollectorWithClient(client)
			metrics, err := collectNVRAMMetrics(t, col, context.Background())
			if err != nil {
				t.Fatalf("col.Update 发生意外错误: %v", err)
			}
			modeList, ok := metrics["router_network_mode"]
			if !ok || len(modeList) != 1 {
				t.Fatalf("未找到或存在多条 router_network_mode 指标")
			}
			if modeList[0].labels["mode"] != tc.expected {
				t.Errorf("sw_mode=%q 映射错误, 期望: %s, 实际: %s", tc.swMode, tc.expected, modeList[0].labels["mode"])
			}
		})
	}
}

// TestNVRAMCollector_FirmwareVersionFormatting 验证各种固件主次版本号格式拼接
func TestNVRAMCollector_FirmwareVersionFormatting(t *testing.T) {
	tests := []struct {
		name     string
		buildno  string
		extendno string
		expected string
	}{
		{
			name:     "正常 buildno 与 extendno 拼接",
			buildno:  "386.4",
			extendno: "0",
			expected: "386.4_0",
		},
		{
			name:     "extendno 自带下划线前缀",
			buildno:  "388.1",
			extendno: "_2",
			expected: "388.1_2",
		},
		{
			name:     "仅有 buildno",
			buildno:  "3004.388.4",
			extendno: "",
			expected: "3004.388.4",
		},
		{
			name:     "仅有 extendno",
			buildno:  "",
			extendno: "20558",
			expected: "20558",
		},
		{
			name:     "两者均为空",
			buildno:  "",
			extendno: "",
			expected: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := nvram.NewMockClient(map[string]string{
				"buildno":  tc.buildno,
				"extendno": tc.extendno,
			})
			col := NewNVRAMCollectorWithClient(client)
			metrics, err := collectNVRAMMetrics(t, col, context.Background())
			if err != nil {
				t.Fatalf("col.Update 发生意外错误: %v", err)
			}
			infoList, ok := metrics["router_info"]
			if !ok || len(infoList) != 1 {
				t.Fatalf("未找到或存在多条 router_info 指标")
			}
			if got := infoList[0].labels["firmware_version"]; got != tc.expected {
				t.Errorf("firmware_version 不匹配, 期望: %s, 实际: %s", tc.expected, got)
			}
		})
	}
}

// TestNVRAMCollector_DisconnectedState 验证 WAN 口未连通及无外网访问状态下的指标
func TestNVRAMCollector_DisconnectedState(t *testing.T) {
	mockData := map[string]string{
		"productid":     "GT-AX6000",
		"buildno":       "3004.388.5",
		"sw_mode":       "1",
		"lan_ipaddr":    "192.168.1.1",
		"lan_netmask":   "255.255.255.0",
		"wan0_ipaddr":   "0.0.0.0",
		"wan0_gateway":  "",
		"wan0_dns":      "",
		"wan0_state_t":  "0", // 断开
		"wan0_proto":    "pppoe",
		"wan0_uptime":   "0",
		"link_internet": "0", // 无网络连接
		"ntp_ready":     "0",
	}
	client := nvram.NewMockClient(mockData)
	col := NewNVRAMCollectorWithClient(client)

	metrics, err := collectNVRAMMetrics(t, col, context.Background())
	if err != nil {
		t.Fatalf("col.Update 发生意外错误: %v", err)
	}

	// wan0_state_t != 2 -> router_wan_status == 0
	if ws := metrics["router_wan_status"][0]; ws.value != 0.0 {
		t.Errorf("router_wan_status 断线状态应为 0.0, 实际为: %f", ws.value)
	}
	// link_internet != 2 -> router_wan_internet_status == 0
	if wis := metrics["router_wan_internet_status"][0]; wis.value != 0.0 {
		t.Errorf("router_wan_internet_status 断网状态应为 0.0, 实际为: %f", wis.value)
	}
	// wan0_uptime == 0
	if wu := metrics["router_wan_uptime_seconds"][0]; wu.value != 0.0 {
		t.Errorf("router_wan_uptime_seconds 应为 0.0, 实际为: %f", wu.value)
	}
	// ntp_ready != 1 -> router_ntp_synced == 0
	if ntp := metrics["router_ntp_synced"][0]; ntp.value != 0.0 {
		t.Errorf("router_ntp_synced 未同步状态应为 0.0, 实际为: %f", ntp.value)
	}
}

// TestNVRAMCollector_NTPUnsynced 验证 NTP 不同状态值下的指标暴露
func TestNVRAMCollector_NTPUnsynced(t *testing.T) {
	tests := []struct {
		ntpReady string
		expected float64
	}{
		{ntpReady: "1", expected: 1.0},
		{ntpReady: "0", expected: 0.0},
		{ntpReady: "2", expected: 0.0},
		{ntpReady: "", expected: 0.0},
		{ntpReady: "true", expected: 0.0},
	}

	for _, tc := range tests {
		t.Run("ntp_ready_"+tc.ntpReady, func(t *testing.T) {
			client := nvram.NewMockClient(map[string]string{
				"ntp_ready": tc.ntpReady,
			})
			col := NewNVRAMCollectorWithClient(client)
			metrics, err := collectNVRAMMetrics(t, col, context.Background())
			if err != nil {
				t.Fatalf("col.Update 发生意外错误: %v", err)
			}
			ntpList, ok := metrics["router_ntp_synced"]
			if !ok || len(ntpList) != 1 {
				t.Fatalf("未找到或存在多条 router_ntp_synced 指标")
			}
			if ntpList[0].value != tc.expected {
				t.Errorf("ntp_ready=%q 期望指标值为 %f, 实际为 %f", tc.ntpReady, tc.expected, ntpList[0].value)
			}
		})
	}
}

// TestNVRAMCollector_MissingKeys 验证 NVRAM 变量缺失场景下的降级回退与安全性
func TestNVRAMCollector_MissingKeys(t *testing.T) {
	// 空数据 map
	client := nvram.NewMockClient(map[string]string{})
	col := NewNVRAMCollectorWithClient(client)

	metrics, err := collectNVRAMMetrics(t, col, context.Background())
	if err != nil {
		t.Fatalf("缺失 NVRAM 变量时 col.Update 不应报错, 实际错误: %v", err)
	}

	// router_info 回退 unknown
	info := metrics["router_info"][0]
	if info.labels["product_id"] != "unknown" {
		t.Errorf("product_id 缺失时应回退为 unknown, 实际: %s", info.labels["product_id"])
	}
	if info.labels["firmware_version"] != "unknown" {
		t.Errorf("firmware_version 缺失时应回退为 unknown, 实际: %s", info.labels["firmware_version"])
	}
	if info.labels["architecture"] != runtime.GOARCH {
		t.Errorf("architecture 默认应为 runtime.GOARCH, 实际: %s", info.labels["architecture"])
	}

	// router_network_mode 回退 unknown
	mode := metrics["router_network_mode"][0]
	if mode.labels["mode"] != "unknown" {
		t.Errorf("mode 缺失时应回退为 unknown, 实际: %s", mode.labels["mode"])
	}

	// router_gateway_info 各标签应为空字符串而不是 panic
	gw := metrics["router_gateway_info"][0]
	if gw.labels["lan_ip"] != "" || gw.labels["wan_ip"] != "" {
		t.Errorf("网关 IP 缺失时应为空字符串, 实际: %+v", gw.labels)
	}

	// router_wan_status 回退 0
	if ws := metrics["router_wan_status"][0]; ws.value != 0.0 {
		t.Errorf("router_wan_status 缺失时应为 0.0, 实际: %f", ws.value)
	}
	// router_wan_internet_status 回退 0
	if wis := metrics["router_wan_internet_status"][0]; wis.value != 0.0 {
		t.Errorf("router_wan_internet_status 缺失时应为 0.0, 实际: %f", wis.value)
	}
	// router_wan_uptime_seconds 回退 0
	if wu := metrics["router_wan_uptime_seconds"][0]; wu.value != 0.0 {
		t.Errorf("router_wan_uptime_seconds 缺失时应为 0.0, 实际: %f", wu.value)
	}
	// router_ntp_synced 回退 0
	if ntp := metrics["router_ntp_synced"][0]; ntp.value != 0.0 {
		t.Errorf("router_ntp_synced 缺失时应为 0.0, 实际: %f", ntp.value)
	}
}

// TestNVRAMCollector_InvalidUptimeFormat 验证 wan0_uptime 为非数字格式时的容错
func TestNVRAMCollector_InvalidUptimeFormat(t *testing.T) {
	client := nvram.NewMockClient(map[string]string{
		"wan0_uptime": "invalid_seconds_string",
	})
	col := NewNVRAMCollectorWithClient(client)

	metrics, err := collectNVRAMMetrics(t, col, context.Background())
	if err != nil {
		t.Fatalf("非数字 uptime 格式时不应报错, 实际错误: %v", err)
	}
	if wu := metrics["router_wan_uptime_seconds"][0]; wu.value != 0.0 {
		t.Errorf("异常格式 uptime 应解析回退为 0.0, 实际: %f", wu.value)
	}
}

// TestNVRAMCollector_ContextCancelled 验证外部 context 取消时的即时退出
func TestNVRAMCollector_ContextCancelled(t *testing.T) {
	col := NewNVRAMCollectorWithClient(&blockingNVRAMClient{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	ch := make(chan prometheus.Metric, 10)
	err := col.Update(ctx, ch)
	close(ch)

	if err == nil {
		t.Fatalf("当 Context 被取消时, col.Update 应返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("期望错误为 context.Canceled, 实际得到: %v", err)
	}
}

// TestNVRAMCollector_ClientError 验证底层 NVRAM Client 查询失败时的错误传播
func TestNVRAMCollector_ClientError(t *testing.T) {
	col := NewNVRAMCollectorWithClient(&nvramFailingClient{})

	ch := make(chan prometheus.Metric, 10)
	err := col.Update(context.Background(), ch)
	close(ch)

	if err == nil {
		t.Fatalf("底层 NVRAM 失败时, col.Update 应返回错误")
	}
	if !strings.Contains(err.Error(), "nvram") {
		t.Errorf("错误信息应提及 nvram, 实际: %v", err)
	}
}

// TestNVRAMCollector_NilClientFallback 验证传入 nil Client 时的安全回退
func TestNVRAMCollector_NilClientFallback(t *testing.T) {
	col := NewNVRAMCollectorWithClient(nil)
	if col == nil {
		t.Fatalf("NewNVRAMCollectorWithClient(nil) 不应返回 nil")
	}
	if col.Name() != "nvram" {
		t.Errorf("采集器名称应为 nvram, 实际为: %s", col.Name())
	}
}

// TestNVRAMCollector_RegistrationAndFactory 验证采集器注册中心及工厂函数调度
func TestNVRAMCollector_RegistrationAndFactory(t *testing.T) {
	RegisterNVRAMCollector()

	// 验证工厂函数
	col, err := NewNVRAMCollector()
	if err != nil {
		t.Fatalf("NewNVRAMCollector() 工厂创建失败: %v", err)
	}
	if col.Name() != "nvram" {
		t.Errorf("Name() 期望: nvram, 实际: %s", col.Name())
	}

	// 验证在 AvailableCollectors 中存在且默认启用
	avail := AvailableCollectors()
	enabled, exists := avail["nvram"]
	if !exists {
		t.Fatalf("采集器 nvram 未在 AvailableCollectors 中注册")
	}
	if !enabled {
		t.Errorf("采集器 nvram 默认状态应为 true (启用)")
	}

	// 验证集成至 MerlinCollector 调度器
	merlin, err := NewMerlinCollector(1*time.Second, map[string]bool{"nvram": true})
	if err != nil {
		t.Fatalf("初始化包含 nvram 的 MerlinCollector 失败: %v", err)
	}
	if merlin == nil {
		t.Fatalf("MerlinCollector 实例为空")
	}
}
