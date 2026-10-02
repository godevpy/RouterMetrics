// Package collector 定义了指标采集器的通用接口、注册中心及 MerlinCollector 聚合调度器。
package collector

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Namespace 是所有暴露指标的前缀命名空间
const Namespace = "router"

var (
	// scrapeDurationDesc 记录每个子采集器的执行耗时
	scrapeDurationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "exporter", "scrape_duration_seconds"),
		"Scrape duration of each collector in seconds.",
		[]string{"collector"}, nil,
	)
	// scrapeSuccessDesc 记录每个子采集器是否成功执行 (1 为成功，0 为失败)
	scrapeSuccessDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "exporter", "collector_success"),
		"Whether a collector scrape succeeded (1) or failed (0).",
		[]string{"collector"}, nil,
	)
	// lastScrapeTimestampDesc 记录最后一次抓取完成的 Unix 时间戳 (秒)
	lastScrapeTimestampDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "exporter", "last_scrape_timestamp_seconds"),
		"Unix timestamp in seconds when the exporter last scraped metrics.",
		nil, nil,
	)
)

// Collector 定义所有子指标采集器必须实现的统一接口
type Collector interface {
	// Name 返回采集器唯一标识名称
	Name() string
	// Update 在传入的 context 约束下执行指标抓取，并将指标推入 ch
	Update(ctx context.Context, ch chan<- prometheus.Metric) error
}

// Factory 是创建 Collector 实例的工厂函数类型
type Factory func() (Collector, error)

var (
	factories       = make(map[string]Factory)
	defaultStatuses = make(map[string]bool)
	factoriesMu     sync.RWMutex
)

// RegisterCollector 注册采集器工厂函数及默认启用状态
func RegisterCollector(name string, defaultState bool, factory Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[name] = factory
	defaultStatuses[name] = defaultState
}

// AvailableCollectors 返回所有已注册采集器的名称及其默认启用状态
func AvailableCollectors() map[string]bool {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	res := make(map[string]bool, len(defaultStatuses))
	for k, v := range defaultStatuses {
		res[k] = v
	}
	return res
}

// MerlinCollector 实现了 prometheus.Collector 接口，用于并发调度管理所有启用的子采集器
type MerlinCollector struct {
	collectors    map[string]Collector
	scrapeTimeout time.Duration
}

// NewMerlinCollector 实例化 MerlinCollector，根据传入的超时时间及启用状态表初始化启用的子采集器
func NewMerlinCollector(timeout time.Duration, enabled map[string]bool) (*MerlinCollector, error) {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()

	active := make(map[string]Collector)
	for name, factory := range factories {
		isEnabled := defaultStatuses[name]
		if override, ok := enabled[name]; ok {
			isEnabled = override
		}
		if isEnabled {
			col, err := factory()
			if err != nil {
				return nil, fmt.Errorf("初始化采集器 %s 失败: %w", name, err)
			}
			active[name] = col
		}
	}

	return &MerlinCollector{
		collectors:    active,
		scrapeTimeout: timeout,
	}, nil
}

// Describe 向 Prometheus 发送 Exporter 自身内部状态指标的描述符
func (m *MerlinCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- scrapeDurationDesc
	ch <- scrapeSuccessDesc
	ch <- lastScrapeTimestampDesc
}

// Collect 并发调度所有启用的子采集器，隔离执行错误并统计耗时和状态指标
func (m *MerlinCollector) Collect(ch chan<- prometheus.Metric) {
	now := float64(time.Now().Unix())
	ch <- prometheus.MustNewConstMetric(lastScrapeTimestampDesc, prometheus.GaugeValue, now)

	ctx, cancel := context.WithTimeout(context.Background(), m.scrapeTimeout)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(len(m.collectors))

	for name, col := range m.collectors {
		go func(n string, c Collector) {
			defer wg.Done()
			start := time.Now()
			err := c.Update(ctx, ch)
			duration := time.Since(start).Seconds()

			ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, duration, n)
			success := 1.0
			if err != nil {
				success = 0.0
				log.Printf("[ERROR] 采集器 %s 采集失败: %v", n, err)
			}
			ch <- prometheus.MustNewConstMetric(scrapeSuccessDesc, prometheus.GaugeValue, success, n)
		}(name, col)
	}

	wg.Wait()
}
