// Package collector 实现各类系统的 Prometheus 指标采集器。
package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"metrics/pkg/nvram"
	"metrics/pkg/util"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultLeasesPath    = "/var/lib/misc/dnsmasq.leases"
	defaultARPPath       = "/proc/net/arp"
	defaultLeaseDuration = 86400
)

var (
	// lanClientInfoDesc 描述局域网终端的基础信息与连接模式 (值为 1.0)
	lanClientInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "lan_client", "info"),
		"LAN client basic metadata and connection details.",
		[]string{"ip", "mac", "hostname", "is_dhcp", "type"}, nil,
	)

	// lanClientLeaseExpiresDesc 描述 DHCP 客户端租约到期时间戳 (秒)
	lanClientLeaseExpiresDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "lan_client", "lease_expires_timestamp_seconds"),
		"LAN client DHCP lease expiration timestamp in seconds.",
		[]string{"ip", "mac"}, nil,
	)

	// lanClientLastHeartbeatDesc 描述 DHCP 客户端上次心跳时间戳 (秒)
	lanClientLastHeartbeatDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "lan_client", "last_heartbeat_timestamp_seconds"),
		"LAN client DHCP last heartbeat timestamp in seconds.",
		[]string{"ip", "mac"}, nil,
	)

	// lanClientActiveDesc 描述客户端在 ARP 缓存表中的活跃连通状态 (1 为活跃，0 为不活跃)
	lanClientActiveDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "lan_client", "active"),
		"LAN client active ARP status (1 for active, 0 for inactive).",
		[]string{"ip", "mac"}, nil,
	)

	// lanClientReceiveBytesDesc 描述终端接收字节总数 (终端下行流量)
	lanClientReceiveBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "lan_client", "receive_bytes_total"),
		"LAN client receive bytes total (downlink from AP).",
		[]string{"ip", "mac"}, nil,
	)

	// lanClientTransmitBytesDesc 描述终端发送字节总数 (终端上行流量)
	lanClientTransmitBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "lan_client", "transmit_bytes_total"),
		"LAN client transmit bytes total (uplink to AP).",
		[]string{"ip", "mac"}, nil,
	)

	// LANClientTrafficFlag 控制是否采集 Wi-Fi 终端流量统计 (对应 --collector.lan-client.traffic)
	LANClientTrafficFlag = false

	// rxBytesRegex 匹配终端上行接收字节（如 rx data bytes: 1234 或 rx_bytes: 1234）
	rxBytesRegex = regexp.MustCompile(`(?i)(?:rx(?:_|\s+data\s+|\s+)bytes)\s*[:=]?\s*(\d+)`)

	// txBytesRegex 匹配终端下行发送字节（如 tx data bytes: 1234 或 tx_bytes: 1234）
	txBytesRegex = regexp.MustCompile(`(?i)(?:tx(?:_|\s+data\s+|\s+)bytes)\s*[:=]?\s*(\d+)`)
)

type leaseEntry struct {
	ExpiresAt int64
	MAC       string
	IP        string
	Hostname  string
}

type arpEntry struct {
	IP     string
	HWType string
	Flags  string
	MAC    string
	Mask   string
	Device string
}

type lanClient struct {
	MAC           string
	IP            string
	Hostname      string
	IsDHCP        bool
	LeaseExpires  int64
	LastHeartbeat int64
	Active        float64
	ConnType      string
	wifiIface     string
	RxBytes       float64
	TxBytes       float64
	HasTraffic    bool
}

// lanClientCollector 采集局域网终端元数据、心跳续租与流量统计
type lanClientCollector struct {
	leasesPath     string
	arpPath        string
	nvramClient    nvram.Client
	cmdRunner      func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error)
	cmdTimeout     time.Duration
	collectTraffic bool
}

// RegisterLANClientCollector 将局域网终端与心跳采集器注册至注册中心
func RegisterLANClientCollector() {
	RegisterCollector("lan_client", true, NewLANClientCollector)
}

func init() {
	RegisterLANClientCollector()
}

// NewLANClientCollector 创建默认配置的局域网终端采集器
func NewLANClientCollector() (Collector, error) {
	return NewLANClientCollectorWithConfig(
		defaultLeasesPath,
		defaultARPPath,
		nvram.NewClient(800*time.Millisecond),
		util.RunCommandWithTimeout,
		LANClientTrafficFlag,
	), nil
}

// NewLANClientCollectorWithConfig 支持依赖注入的局域网终端采集器构造函数
func NewLANClientCollectorWithConfig(
	leasesPath string,
	arpPath string,
	nvramClient nvram.Client,
	cmdRunner func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error),
	collectTraffic bool,
) Collector {
	if leasesPath == "" {
		leasesPath = defaultLeasesPath
	}
	if arpPath == "" {
		arpPath = defaultARPPath
	}
	if nvramClient == nil {
		nvramClient = nvram.NewClient(800 * time.Millisecond)
	}
	if cmdRunner == nil {
		cmdRunner = util.RunCommandWithTimeout
	}
	return &lanClientCollector{
		leasesPath:     leasesPath,
		arpPath:        arpPath,
		nvramClient:    nvramClient,
		cmdRunner:      cmdRunner,
		cmdTimeout:     800 * time.Millisecond,
		collectTraffic: collectTraffic,
	}
}

// Name 返回采集器唯一标识名称
func (c *lanClientCollector) Name() string {
	return "lan_client"
}

// Update 执行局域网终端状态与心跳抓取流程
func (c *lanClientCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// 1. 读取并解析 dnsmasq.leases 文件
	leasesFile, err := os.Open(c.leasesPath)
	if err != nil {
		return fmt.Errorf("打开 dnsmasq.leases 文件 %s 失败: %w", c.leasesPath, err)
	}
	defer leasesFile.Close()

	leases, err := parseDNSMasqLeases(leasesFile)
	if err != nil {
		return fmt.Errorf("解析 dnsmasq.leases 失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 读取并解析 /proc/net/arp 文件
	arpFile, err := os.Open(c.arpPath)
	if err != nil {
		return fmt.Errorf("打开 arp 文件 %s 失败: %w", c.arpPath, err)
	}
	defer arpFile.Close()

	arpEntries, err := parseARPTable(arpFile)
	if err != nil {
		return fmt.Errorf("解析 arp 表失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 3. 从 NVRAM 批量读取租约时长及无线接口配置
	leaseDuration := int64(defaultLeaseDuration)
	wifiIfaces := make([]string, 0, 3)

	if c.nvramClient != nil {
		nvramKeys := []string{"dhcp_lease", "wl0_ifname", "wl1_ifname", "wl2_ifname"}
		nvramMap, err := c.nvramClient.GetAll(ctx, nvramKeys)
		if err != nil {
			log.Printf("[WARN] 查询 NVRAM 局域网/无线配置失败: %v", err)
		} else {
			if leaseStr := strings.TrimSpace(nvramMap["dhcp_lease"]); leaseStr != "" {
				if d, parseErr := strconv.ParseInt(leaseStr, 10, 64); parseErr == nil && d > 0 {
					leaseDuration = d
				}
			}
			for _, key := range []string{"wl0_ifname", "wl1_ifname", "wl2_ifname"} {
				if iface := strings.TrimSpace(nvramMap[key]); iface != "" {
					wifiIfaces = append(wifiIfaces, iface)
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 4. 融合 DHCP 租约与 ARP 表
	clients := make(map[string]*lanClient)

	// 首先注入 DHCP 客户端
	for _, l := range leases {
		mac := l.MAC
		if mac == "" {
			continue
		}
		lastHb := l.ExpiresAt - leaseDuration
		if lastHb < 0 {
			lastHb = 0
		}
		// 若同一 MAC 存在多条记录，保留到期时间更晚的记录
		if existing, ok := clients[mac]; ok {
			if l.ExpiresAt <= existing.LeaseExpires {
				continue
			}
		}
		clients[mac] = &lanClient{
			MAC:           mac,
			IP:            l.IP,
			Hostname:      l.Hostname,
			IsDHCP:        true,
			LeaseExpires:  l.ExpiresAt,
			LastHeartbeat: lastHb,
			Active:        0.0,
			ConnType:      "wired",
		}
	}

	// 融合 ARP 表：标记活跃状态并纳入静态客户端
	for _, entry := range arpEntries {
		mac := entry.MAC
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}
		active := isARPActive(entry.Flags)
		if client, ok := clients[mac]; ok {
			if active {
				client.Active = 1.0
			}
			if client.IP == "" {
				client.IP = entry.IP
			}
		} else if active {
			// 不在 DHCP 租约中但在 ARP 中有效活跃的设备为静态客户端
			clients[mac] = &lanClient{
				MAC:      mac,
				IP:       entry.IP,
				Hostname: "",
				IsDHCP:   false,
				Active:   1.0,
				ConnType: "wired",
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 5. 探测无线接口并匹配客户端连接类型
	wifiClientIfaces := make(map[string]string)
	for _, iface := range wifiIfaces {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := c.cmdRunner(ctx, c.cmdTimeout, "wl", "-i", iface, "assoclist")
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[WARN] 查询 Wi-Fi 接口 %s 关联列表失败: %v", iface, err)
			continue
		}
		macs := parseAssoclist(string(out))
		for _, mac := range macs {
			wifiClientIfaces[mac] = iface
		}
	}

	for _, client := range clients {
		if iface, ok := wifiClientIfaces[client.MAC]; ok {
			client.ConnType = "wifi"
			client.wifiIface = iface
		} else {
			client.ConnType = "wired"
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 6. 若启用了终端流量统计，针对 Wi-Fi 客户端采集 sta_info
	if c.collectTraffic {
		for _, client := range clients {
			if client.ConnType != "wifi" || client.wifiIface == "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			out, err := c.cmdRunner(ctx, c.cmdTimeout, "wl", "-i", client.wifiIface, "sta_info", client.MAC)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Printf("[WARN] 采集终端 %s sta_info 失败: %v", client.MAC, err)
				continue
			}
			rxBytes, txBytes, err := parseStaInfo(string(out))
			if err != nil {
				log.Printf("[WARN] 解析终端 %s sta_info 流量失败: %v", client.MAC, err)
				continue
			}
			client.RxBytes = rxBytes
			client.TxBytes = txBytes
			client.HasTraffic = true
		}
	}

	// 7. 排序并推入 Prometheus 指标通道
	clientList := make([]*lanClient, 0, len(clients))
	for _, client := range clients {
		clientList = append(clientList, client)
	}
	sort.Slice(clientList, func(i, j int) bool {
		if clientList[i].IP != clientList[j].IP {
			return clientList[i].IP < clientList[j].IP
		}
		return clientList[i].MAC < clientList[j].MAC
	})

	for _, client := range clientList {
		isDHCPStr := "false"
		if client.IsDHCP {
			isDHCPStr = "true"
		}

		ch <- prometheus.MustNewConstMetric(
			lanClientInfoDesc,
			prometheus.GaugeValue,
			1.0,
			client.IP,
			client.MAC,
			client.Hostname,
			isDHCPStr,
			client.ConnType,
		)

		if client.IsDHCP {
			ch <- prometheus.MustNewConstMetric(
				lanClientLeaseExpiresDesc,
				prometheus.GaugeValue,
				float64(client.LeaseExpires),
				client.IP,
				client.MAC,
			)
			ch <- prometheus.MustNewConstMetric(
				lanClientLastHeartbeatDesc,
				prometheus.GaugeValue,
				float64(client.LastHeartbeat),
				client.IP,
				client.MAC,
			)
		}

		ch <- prometheus.MustNewConstMetric(
			lanClientActiveDesc,
			prometheus.GaugeValue,
			client.Active,
			client.IP,
			client.MAC,
		)

		if client.HasTraffic {
			ch <- prometheus.MustNewConstMetric(
				lanClientReceiveBytesDesc,
				prometheus.CounterValue,
				client.RxBytes,
				client.IP,
				client.MAC,
			)
			ch <- prometheus.MustNewConstMetric(
				lanClientTransmitBytesDesc,
				prometheus.CounterValue,
				client.TxBytes,
				client.IP,
				client.MAC,
			)
		}
	}

	return nil
}

// parseDNSMasqLeases 逐行流式解析 dnsmasq.leases 文件
func parseDNSMasqLeases(r io.Reader) ([]*leaseEntry, error) {
	scanner := bufio.NewScanner(r)
	var leases []*leaseEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		expiresAt, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		leases = append(leases, &leaseEntry{
			ExpiresAt: expiresAt,
			MAC:       strings.ToLower(fields[1]),
			IP:        fields[2],
			Hostname:  fields[3],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return leases, nil
}

// parseARPTable 逐行流式解析 /proc/net/arp 表
func parseARPTable(r io.Reader) ([]*arpEntry, error) {
	scanner := bufio.NewScanner(r)
	var entries []*arpEntry
	firstLine := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if firstLine {
			firstLine = false
			if strings.HasPrefix(line, "IP") {
				continue
			}
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		entries = append(entries, &arpEntry{
			IP:     fields[0],
			HWType: fields[1],
			Flags:  fields[2],
			MAC:    strings.ToLower(fields[3]),
			Mask:   fields[4],
			Device: fields[5],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// isARPActive 判定 ARP 条目标志是否为有效活跃连通状态
func isARPActive(flags string) bool {
	f := strings.TrimSpace(flags)
	return f != "" && f != "0x0" && f != "0x00" && f != "0"
}

// parseStaInfo 从 wl sta_info 输出中提取终端流量指标。
// 注：在无线 AP 驱动视角中，下行流量（终端接收）由 AP 发送（tx data bytes），
// 上行流量（终端发送）由 AP 接收（rx data bytes）。
// 因此：
// rxBytes (终端接收) 对应驱动的 tx data bytes (下行)
// txBytes (终端发送) 对应驱动的 rx data bytes (上行)
func parseStaInfo(output string) (rxBytes float64, txBytes float64, err error) {
	txMatch := txBytesRegex.FindStringSubmatch(output)
	rxMatch := rxBytesRegex.FindStringSubmatch(output)

	if len(txMatch) < 2 && len(rxMatch) < 2 {
		return 0, 0, fmt.Errorf("未能从 sta_info 输出中提取到流量统计数据")
	}

	if len(txMatch) >= 2 {
		val, parseErr := strconv.ParseFloat(txMatch[1], 64)
		if parseErr == nil {
			rxBytes = val
		}
	}

	if len(rxMatch) >= 2 {
		val, parseErr := strconv.ParseFloat(rxMatch[1], 64)
		if parseErr == nil {
			txBytes = val
		}
	}

	return rxBytes, txBytes, nil
}
