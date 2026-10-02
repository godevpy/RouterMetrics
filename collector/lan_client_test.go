// Package collector 实现各类系统的 Prometheus 指标采集器。
package collector

import (
	"context"
	"errors"
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

type lanMetricData struct {
	labels map[string]string
	value  float64
}

func collectLANMetrics(t *testing.T, col Collector, ctx context.Context) (map[string][]lanMetricData, error) {
	t.Helper()
	ch := make(chan prometheus.Metric, 50)
	err := col.Update(ctx, ch)
	close(ch)

	res := make(map[string][]lanMetricData)
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
		res[name] = append(res[name], lanMetricData{
			labels: labels,
			value:  val,
		})
	}
	return res, err
}

func TestParseDNSMasqLeases(t *testing.T) {
	content := `
# 模拟 dnsmasq.leases 内容
1727823600 00:11:22:33:44:55 192.168.50.101 iPhone-13 01:00:11:22:33:44:55
1727824000 AA:BB:CC:DD:EE:FF 192.168.50.102 * *
1727824500 11:22:33:44:55:66 192.168.50.103 MacBook-Pro

# 异常短行与非数字时间戳行，应被安全跳过
bad_line
not_a_ts 00:00:00:00:00:00 192.168.50.104 test
`
	leases, err := parseDNSMasqLeases(strings.NewReader(content))
	if err != nil {
		t.Fatalf("parseDNSMasqLeases 发生非预期错误: %v", err)
	}

	if len(leases) != 3 {
		t.Fatalf("预期解析 3 条有效租约，实际获得 %d 条", len(leases))
	}

	expected := []struct {
		expiresAt int64
		mac       string
		ip        string
		hostname  string
	}{
		{expiresAt: 1727823600, mac: "00:11:22:33:44:55", ip: "192.168.50.101", hostname: "iPhone-13"},
		{expiresAt: 1727824000, mac: "aa:bb:cc:dd:ee:ff", ip: "192.168.50.102", hostname: "*"},
		{expiresAt: 1727824500, mac: "11:22:33:44:55:66", ip: "192.168.50.103", hostname: "MacBook-Pro"},
	}

	for i, exp := range expected {
		actual := leases[i]
		if actual.ExpiresAt != exp.expiresAt || actual.MAC != exp.mac || actual.IP != exp.ip || actual.Hostname != exp.hostname {
			t.Errorf("条目 [%d] 解析不匹配: 预期 %+v, 实际 %+v", i, exp, actual)
		}
	}
}

func TestParseARPTable(t *testing.T) {
	content := `IP address       HW type     Flags       HW address            Mask     Device
192.168.50.1     0x1         0x2         00:aa:bb:cc:dd:ee     *        br0
192.168.50.101   0x1         0x2         00:11:22:33:44:55     *        br0
192.168.50.102   0x1         0x0         00:00:00:00:00:00     *        br0
192.168.50.105   0x1         0x2         22:33:44:55:66:77     *        br0

# 短行与空行测试
invalid_arp_line
`
	entries, err := parseARPTable(strings.NewReader(content))
	if err != nil {
		t.Fatalf("parseARPTable 发生非预期错误: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("预期解析 4 条有效 ARP 记录，实际获得 %d 条", len(entries))
	}

	if entries[0].IP != "192.168.50.1" || entries[0].Flags != "0x2" || entries[0].MAC != "00:aa:bb:cc:dd:ee" {
		t.Errorf("条目 [0] 解析不匹配: %+v", entries[0])
	}
	if entries[2].Flags != "0x0" {
		t.Errorf("条目 [2] Flags 预期 0x0, 实际 %s", entries[2].Flags)
	}
}

func TestParseStaInfo(t *testing.T) {
	testCases := []struct {
		name        string
		output      string
		expectedRx  float64
		expectedTx  float64
		expectError bool
	}{
		{
			name: "Broadcom 典型 wl sta_info 输出格式",
			output: `sta_info 00:11:22:33:44:55
  idle 0
  in network 3600
  state: AUTHENTICATED ASSOCIATED AUTHORIZED
  flags: 0x1
  HT caps: 0x2
  tx data pkts: 1500
  tx data bytes: 1048576
  rx data pkts: 800
  rx data bytes: 524288
`,
			expectedRx:  1048576, // 终端下行/接收对应驱动 tx
			expectedTx:  524288,  // 终端上行/发送对应驱动 rx
			expectError: false,
		},
		{
			name: "下划线格式 tx_bytes 与 rx_bytes",
			output: `
tx_bytes: 204800
rx_bytes: 102400
`,
			expectedRx:  204800,
			expectedTx:  102400,
			expectError: false,
		},
		{
			name: "等号与空格格式",
			output: `
tx bytes = 8192
rx bytes = 4096
`,
			expectedRx:  8192,
			expectedTx:  4096,
			expectError: false,
		},
		{
			name:        "无流量数据的空输出",
			output:      "unknown sta_info output\nno data here",
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rx, tx, err := parseStaInfo(tc.output)
			if tc.expectError {
				if err == nil {
					t.Fatalf("预期返回错误，但实际成功返回: rx=%.0f, tx=%.0f", rx, tx)
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期错误: %v", err)
			}
			if rx != tc.expectedRx || tx != tc.expectedTx {
				t.Errorf("流量解析不匹配: 预期 rx=%.0f, tx=%.0f, 实际 rx=%.0f, tx=%.0f",
					tc.expectedRx, tc.expectedTx, rx, tx)
			}
		})
	}
}

func TestLANClientCollector_FullScrape(t *testing.T) {
	tempDir := t.TempDir()

	leasesPath := filepath.Join(tempDir, "dnsmasq.leases")
	leasesContent := `1727823600 00:11:22:33:44:55 192.168.50.101 iPhone-13 01:00:11:22:33:44:55
1727824000 aa:bb:cc:dd:ee:ff 192.168.50.102 PC-Wired *
`
	if err := os.WriteFile(leasesPath, []byte(leasesContent), 0644); err != nil {
		t.Fatalf("写入测试 leases 文件失败: %v", err)
	}

	arpPath := filepath.Join(tempDir, "arp")
	arpContent := `IP address       HW type     Flags       HW address            Mask     Device
192.168.50.101   0x1         0x2         00:11:22:33:44:55     *        br0
192.168.50.102   0x1         0x0         aa:bb:cc:dd:ee:ff     *        br0
192.168.50.103   0x1         0x2         11:22:33:44:55:66     *        br0
192.168.50.104   0x1         0x0         00:00:00:00:00:00     *        br0
`
	if err := os.WriteFile(arpPath, []byte(arpContent), 0644); err != nil {
		t.Fatalf("写入测试 arp 文件失败: %v", err)
	}

	mockNVRAM := nvram.NewMockClient(map[string]string{
		"dhcp_lease": "86400",
		"wl0_ifname": "eth6",
		"wl1_ifname": "eth7",
		"wl2_ifname": "",
	})

	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if name == "wl" && len(args) >= 3 && args[0] == "-i" && args[2] == "assoclist" {
			if args[1] == "eth6" {
				return []byte("assoclist 00:11:22:33:44:55\n"), nil
			}
			return []byte(""), nil
		}
		if name == "wl" && len(args) >= 4 && args[0] == "-i" && args[2] == "sta_info" {
			if args[3] == "00:11:22:33:44:55" {
				return []byte("tx data bytes: 1048576\nrx data bytes: 524288\n"), nil
			}
		}
		return nil, fmt.Errorf("unknown command: %s %v", name, args)
	}

	collector := NewLANClientCollectorWithConfig(leasesPath, arpPath, mockNVRAM, mockRunner, true)
	if collector.Name() != "lan_client" {
		t.Errorf("采集器名称不符合预期: %s", collector.Name())
	}

	ctx := context.Background()
	metrics, err := collectLANMetrics(t, collector, ctx)
	if err != nil {
		t.Fatalf("Update 执行失败: %v", err)
	}

	// 1. 验证 router_lan_client_info
	// 期望 3 个终端：
	// 00:11:22:33:44:55 (DHCP, Wi-Fi, 活跃 1.0)
	// aa:bb:cc:dd:ee:ff (DHCP, 有线, 不活跃 0.0)
	// 11:22:33:44:55:66 (静态, 有线, 活跃 1.0)
	// 00:00:00:00:00:00 应被忽略
	infoList := metrics["router_lan_client_info"]
	if len(infoList) != 3 {
		t.Fatalf("router_lan_client_info 指标数量预期 3, 实际 %d: %+v", len(infoList), infoList)
	}

	infoByMAC := make(map[string]lanMetricData)
	for _, item := range infoList {
		infoByMAC[item.labels["mac"]] = item
	}

	// 终端 1 (Wi-Fi + DHCP)
	c1, ok := infoByMAC["00:11:22:33:44:55"]
	if !ok {
		t.Fatalf("缺失 00:11:22:33:44:55 的 info 指标")
	}
	if c1.labels["ip"] != "192.168.50.101" || c1.labels["hostname"] != "iPhone-13" ||
		c1.labels["is_dhcp"] != "true" || c1.labels["type"] != "wifi" || c1.value != 1.0 {
		t.Errorf("终端 1 标签不匹配: %+v", c1)
	}

	// 终端 2 (Wired + DHCP + 不活跃)
	c2, ok := infoByMAC["aa:bb:cc:dd:ee:ff"]
	if !ok {
		t.Fatalf("缺失 aa:bb:cc:dd:ee:ff 的 info 指标")
	}
	if c2.labels["ip"] != "192.168.50.102" || c2.labels["hostname"] != "PC-Wired" ||
		c2.labels["is_dhcp"] != "true" || c2.labels["type"] != "wired" || c2.value != 1.0 {
		t.Errorf("终端 2 标签不匹配: %+v", c2)
	}

	// 终端 3 (Wired + Static + 活跃)
	c3, ok := infoByMAC["11:22:33:44:55:66"]
	if !ok {
		t.Fatalf("缺失 11:22:33:44:55:66 的 info 指标")
	}
	if c3.labels["ip"] != "192.168.50.103" || c3.labels["hostname"] != "" ||
		c3.labels["is_dhcp"] != "false" || c3.labels["type"] != "wired" || c3.value != 1.0 {
		t.Errorf("终端 3 标签不匹配: %+v", c3)
	}

	// 2. 验证 router_lan_client_active
	activeList := metrics["router_lan_client_active"]
	if len(activeList) != 3 {
		t.Fatalf("router_lan_client_active 指标数量预期 3, 实际 %d", len(activeList))
	}
	activeByMAC := make(map[string]float64)
	for _, item := range activeList {
		activeByMAC[item.labels["mac"]] = item.value
	}
	if activeByMAC["00:11:22:33:44:55"] != 1.0 {
		t.Errorf("终端 1 active 预期 1.0, 实际 %f", activeByMAC["00:11:22:33:44:55"])
	}
	if activeByMAC["aa:bb:cc:dd:ee:ff"] != 0.0 {
		t.Errorf("终端 2 active 预期 0.0, 实际 %f", activeByMAC["aa:bb:cc:dd:ee:ff"])
	}
	if activeByMAC["11:22:33:44:55:66"] != 1.0 {
		t.Errorf("终端 3 active 预期 1.0, 实际 %f", activeByMAC["11:22:33:44:55:66"])
	}

	// 3. 验证 router_lan_client_lease_expires_timestamp_seconds（仅 DHCP）
	leaseList := metrics["router_lan_client_lease_expires_timestamp_seconds"]
	if len(leaseList) != 2 {
		t.Fatalf("router_lan_client_lease_expires 指标数量预期 2, 实际 %d", len(leaseList))
	}
	leaseByMAC := make(map[string]float64)
	for _, item := range leaseList {
		leaseByMAC[item.labels["mac"]] = item.value
	}
	if leaseByMAC["00:11:22:33:44:55"] != 1727823600 {
		t.Errorf("终端 1 lease_expires 预期 1727823600, 实际 %f", leaseByMAC["00:11:22:33:44:55"])
	}

	// 4. 验证 router_lan_client_last_heartbeat_timestamp_seconds（仅 DHCP）
	heartbeatList := metrics["router_lan_client_last_heartbeat_timestamp_seconds"]
	if len(heartbeatList) != 2 {
		t.Fatalf("router_lan_client_last_heartbeat 指标数量预期 2, 实际 %d", len(heartbeatList))
	}
	heartbeatByMAC := make(map[string]float64)
	for _, item := range heartbeatList {
		heartbeatByMAC[item.labels["mac"]] = item.value
	}
	expectedHeartbeat1 := float64(1727823600 - 86400)
	if heartbeatByMAC["00:11:22:33:44:55"] != expectedHeartbeat1 {
		t.Errorf("终端 1 last_heartbeat 预期 %f, 实际 %f", expectedHeartbeat1, heartbeatByMAC["00:11:22:33:44:55"])
	}

	// 5. 验证流量指标（仅 Wi-Fi 终端）
	rxList := metrics["router_lan_client_receive_bytes_total"]
	txList := metrics["router_lan_client_transmit_bytes_total"]
	if len(rxList) != 1 || len(txList) != 1 {
		t.Fatalf("Wi-Fi 流量指标数量不符合预期: rx=%d, tx=%d", len(rxList), len(txList))
	}
	if rxList[0].labels["mac"] != "00:11:22:33:44:55" || rxList[0].value != 1048576 {
		t.Errorf("接收流量指标不匹配: %+v", rxList[0])
	}
	if txList[0].labels["mac"] != "00:11:22:33:44:55" || txList[0].value != 524288 {
		t.Errorf("发送流量指标不匹配: %+v", txList[0])
	}
}

func TestLANClientCollector_HeartbeatCalculation(t *testing.T) {
	tempDir := t.TempDir()
	leasesPath := filepath.Join(tempDir, "dnsmasq.leases")
	leasesContent := `1700000000 00:11:22:33:44:55 192.168.50.101 TestDevice *
`
	if err := os.WriteFile(leasesPath, []byte(leasesContent), 0644); err != nil {
		t.Fatalf("写入测试 leases 失败: %v", err)
	}

	arpPath := filepath.Join(tempDir, "arp")
	arpContent := "IP address HW type Flags HW address Mask Device\n"
	if err := os.WriteFile(arpPath, []byte(arpContent), 0644); err != nil {
		t.Fatalf("写入测试 arp 失败: %v", err)
	}

	// 设定租期为 43200 秒 (12小时)
	mockNVRAM := nvram.NewMockClient(map[string]string{
		"dhcp_lease": "43200",
	})

	collector := NewLANClientCollectorWithConfig(leasesPath, arpPath, mockNVRAM, nil, false)
	metrics, err := collectLANMetrics(t, collector, context.Background())
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	hbList := metrics["router_lan_client_last_heartbeat_timestamp_seconds"]
	if len(hbList) != 1 {
		t.Fatalf("last_heartbeat 指标数量预期 1, 实际 %d", len(hbList))
	}
	expectedHeartbeat := float64(1700000000 - 43200)
	if hbList[0].value != expectedHeartbeat {
		t.Errorf("上次心跳推算错误: 预期 %.0f, 实际 %.0f", expectedHeartbeat, hbList[0].value)
	}
}

func TestLANClientCollector_TrafficDisabled(t *testing.T) {
	tempDir := t.TempDir()
	leasesPath := filepath.Join(tempDir, "dnsmasq.leases")
	leasesContent := `1700000000 00:11:22:33:44:55 192.168.50.101 WifiDevice *
`
	_ = os.WriteFile(leasesPath, []byte(leasesContent), 0644)

	arpPath := filepath.Join(tempDir, "arp")
	arpContent := "IP address HW type Flags HW address Mask Device\n192.168.50.101 0x1 0x2 00:11:22:33:44:55 * br0\n"
	_ = os.WriteFile(arpPath, []byte(arpContent), 0644)

	mockNVRAM := nvram.NewMockClient(map[string]string{
		"wl0_ifname": "eth6",
	})

	staInfoCalled := false
	mockRunner := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if name == "wl" && len(args) >= 3 && args[2] == "assoclist" {
			return []byte("assoclist 00:11:22:33:44:55\n"), nil
		}
		if name == "wl" && len(args) >= 3 && args[2] == "sta_info" {
			staInfoCalled = true
			return []byte("tx data bytes: 1000\nrx data bytes: 500\n"), nil
		}
		return nil, nil
	}

	// 明确 collectTraffic = false
	collector := NewLANClientCollectorWithConfig(leasesPath, arpPath, mockNVRAM, mockRunner, false)
	metrics, err := collectLANMetrics(t, collector, context.Background())
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	if staInfoCalled {
		t.Errorf("在 collectTraffic=false 情况下不应调用 sta_info")
	}

	if len(metrics["router_lan_client_receive_bytes_total"]) != 0 || len(metrics["router_lan_client_transmit_bytes_total"]) != 0 {
		t.Errorf("在 collectTraffic=false 时不应暴露流量指标")
	}
}

func TestLANClientCollector_FileErrors(t *testing.T) {
	tempDir := t.TempDir()
	validFile := filepath.Join(tempDir, "valid")
	_ = os.WriteFile(validFile, []byte(""), 0644)

	// 1. 测试 leases 文件不存在
	c1 := NewLANClientCollectorWithConfig("/nonexistent/leases", validFile, nil, nil, false)
	ch := make(chan prometheus.Metric, 5)
	if err := c1.Update(context.Background(), ch); err == nil {
		t.Errorf("leases 文件不存在时预期返回错误，但返回 nil")
	}

	// 2. 测试 arp 文件不存在
	c2 := NewLANClientCollectorWithConfig(validFile, "/nonexistent/arp", nil, nil, false)
	if err := c2.Update(context.Background(), ch); err == nil {
		t.Errorf("arp 文件不存在时预期返回错误，但返回 nil")
	}
}

func TestLANClientCollector_ContextCancelled(t *testing.T) {
	tempDir := t.TempDir()
	leasesPath := filepath.Join(tempDir, "leases")
	_ = os.WriteFile(leasesPath, []byte(""), 0644)
	arpPath := filepath.Join(tempDir, "arp")
	_ = os.WriteFile(arpPath, []byte(""), 0644)

	collector := NewLANClientCollectorWithConfig(leasesPath, arpPath, nil, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 即时取消

	ch := make(chan prometheus.Metric, 5)
	err := collector.Update(ctx, ch)
	if err != context.Canceled {
		t.Fatalf("预期返回 context.Canceled, 实际返回: %v", err)
	}
}

func TestLANClientCollector_RegistrationAndFactory(t *testing.T) {
	RegisterLANClientCollector()

	col, err := NewLANClientCollector()
	if err != nil {
		t.Fatalf("NewLANClientCollector() 工厂函数失败: %v", err)
	}
	if col.Name() != "lan_client" {
		t.Errorf("采集器名称不符合预期: %s", col.Name())
	}

	available := AvailableCollectors()
	enabled, ok := available["lan_client"]
	if !ok {
		t.Fatalf("lan_client 采集器未在 AvailableCollectors 中注册")
	}
	if !enabled {
		t.Errorf("lan_client 采集器默认状态应为 true (启用)")
	}

	// 验证集成至 MerlinCollector 调度器
	merlin, err := NewMerlinCollector(1*time.Second, map[string]bool{"lan_client": true})
	if err != nil {
		t.Fatalf("初始化包含 lan_client 的 MerlinCollector 失败: %v", err)
	}
	if merlin == nil {
		t.Fatalf("MerlinCollector 实例为空")
	}
}

func TestLANClientCollector_EdgeCases(t *testing.T) {
	// 1. 验证默认空参数回退
	defCol := NewLANClientCollectorWithConfig("", "", nil, nil, false).(*lanClientCollector)
	if defCol.leasesPath != defaultLeasesPath || defCol.arpPath != defaultARPPath {
		t.Errorf("默认路径回退错误: leases=%s, arp=%s", defCol.leasesPath, defCol.arpPath)
	}

	tempDir := t.TempDir()
	leasesPath := filepath.Join(tempDir, "leases")
	// 包含相同 MAC 的早期和晚期租约，以及相同 IP 的两个设备测试排序
	leasesContent := `1700000000 00:11:22:33:44:55 192.168.50.100 OldHost *
1700050000 00:11:22:33:44:55 192.168.50.100 NewHost *
1700010000 00:11:22:33:44:55 192.168.50.100 OlderHost *
1700020000 00:11:22:33:44:aa 192.168.50.200 HostA *
1700030000 00:11:22:33:44:bb 192.168.50.200 HostB *
`
	if err := os.WriteFile(leasesPath, []byte(leasesContent), 0644); err != nil {
		t.Fatalf("写入测试 leases 失败: %v", err)
	}

	arpPath := filepath.Join(tempDir, "arp")
	arpContent := `IP address       HW type     Flags       HW address            Mask     Device
192.168.50.100   0x1         0x2         00:11:22:33:44:55     *        br0
192.168.50.200   0x1         0x2         00:11:22:33:44:aa     *        br0
192.168.50.200   0x1         0x2         00:11:22:33:44:bb     *        br0
`
	if err := os.WriteFile(arpPath, []byte(arpContent), 0644); err != nil {
		t.Fatalf("写入测试 arp 失败: %v", err)
	}

	// 2. NVRAM 查询失败与驱动调用错误场景
	failNVRAM := &nvramFailingClient{}

	mockRunnerWithErrors := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if name == "wl" && len(args) >= 3 && args[2] == "assoclist" {
			return nil, errors.New("broadcom assoclist driver error")
		}
		if name == "wl" && len(args) >= 3 && args[2] == "sta_info" {
			return nil, errors.New("broadcom sta_info timeout")
		}
		return nil, nil
	}

	col := NewLANClientCollectorWithConfig(leasesPath, arpPath, failNVRAM, mockRunnerWithErrors, true)
	metrics, err := collectLANMetrics(t, col, context.Background())
	if err != nil {
		t.Fatalf("即使 NVRAM 和驱动报错，Update 仍应成功完成: %v", err)
	}

	// 验证重复 MAC 覆盖到期时间更晚的记录 (1700050000 而非 1700000000 或 1700010000)
	expiresList := metrics["router_lan_client_lease_expires_timestamp_seconds"]
	var foundRenewed bool
	for _, m := range expiresList {
		if m.labels["mac"] == "00:11:22:33:44:55" {
			if m.value != 1700050000 {
				t.Errorf("重复 MAC 应保留到期时间更晚的租约，实际: %f", m.value)
			}
			foundRenewed = true
		}
	}
	if !foundRenewed {
		t.Errorf("未找到 00:11:22:33:44:55 租约指标")
	}

	// 验证 sta_info 返回无法解析格式时的容错
	mockRunnerBadOutput := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if name == "wl" && len(args) >= 3 && args[2] == "assoclist" {
			return []byte("assoclist 00:11:22:33:44:55\n"), nil
		}
		if name == "wl" && len(args) >= 3 && args[2] == "sta_info" {
			return []byte("garbage sta_info without bytes\n"), nil
		}
		return nil, nil
	}
	mockNVRAM := nvram.NewMockClient(map[string]string{
		"wl0_ifname": "eth6",
	})
	colBadTraffic := NewLANClientCollectorWithConfig(leasesPath, arpPath, mockNVRAM, mockRunnerBadOutput, true)
	metricsBadTraffic, err := collectLANMetrics(t, colBadTraffic, context.Background())
	if err != nil {
		t.Fatalf("解析错误时仍应优雅降级: %v", err)
	}
	if len(metricsBadTraffic["router_lan_client_receive_bytes_total"]) != 0 {
		t.Errorf("sta_info 垃圾输出时不应暴露接收流量指标")
	}

	// 3. 测试中途 Context 取消 (在 assoclist 或 sta_info 执行时已取消)
	ctxAssocCancel, cancelAssoc := context.WithCancel(context.Background())
	runnerCancelOnAssoc := func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
		if name == "wl" && len(args) >= 3 && args[2] == "assoclist" {
			cancelAssoc() // 取消上下文
			return nil, ctx.Err()
		}
		return nil, nil
	}
	colCancelAssoc := NewLANClientCollectorWithConfig(leasesPath, arpPath, mockNVRAM, runnerCancelOnAssoc, true)
	chCancel := make(chan prometheus.Metric, 10)
	if err := colCancelAssoc.Update(ctxAssocCancel, chCancel); err != context.Canceled {
		t.Errorf("在 assoclist 中途取消时应返回 context.Canceled, 实际: %v", err)
	}
}

