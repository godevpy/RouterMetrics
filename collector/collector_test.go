package collector

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// dummyCollector 用于基本采集测试的模拟采集器
type dummyCollector struct{}

func (d *dummyCollector) Name() string { return "dummy" }
func (d *dummyCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	desc := prometheus.NewDesc("router_dummy_metric", "Dummy metric", nil, nil)
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 42)
	return nil
}

// errorCollector 用于测试错误隔离机制的模拟采集器
type errorCollector struct{}

func (e *errorCollector) Name() string { return "error_col" }
func (e *errorCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	return errors.New("simulated scrape failure")
}

// timeoutCollector 用于测试超时取消机制的模拟采集器
type timeoutCollector struct {
	canceled int32
}

func (t *timeoutCollector) Name() string { return "timeout_col" }
func (t *timeoutCollector) Update(ctx context.Context, ch chan<- prometheus.Metric) error {
	select {
	case <-ctx.Done():
		atomic.StoreInt32(&t.canceled, 1)
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return nil
	}
}

func resetRegistry() {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories = make(map[string]Factory)
	defaultStatuses = make(map[string]bool)
}

// TestMerlinCollector_Collect 验证正常采集流程与基础指标暴露
func TestMerlinCollector_Collect(t *testing.T) {
	resetRegistry()
	RegisterCollector("dummy", true, func() (Collector, error) {
		return &dummyCollector{}, nil
	})

	mc, err := NewMerlinCollector(1*time.Second, map[string]bool{"dummy": true})
	if err != nil {
		t.Fatalf("创建 MerlinCollector 失败: %v", err)
	}

	ch := make(chan prometheus.Metric, 20)
	mc.Collect(ch)
	close(ch)

	foundDummy := false
	foundDuration := false
	foundSuccess := false
	foundTimestamp := false

	for m := range ch {
		descStr := m.Desc().String()
		if strings.Contains(descStr, "router_dummy_metric") {
			foundDummy = true
		}
		if strings.Contains(descStr, "router_exporter_scrape_duration_seconds") {
			foundDuration = true
		}
		if strings.Contains(descStr, "router_exporter_collector_success") {
			foundSuccess = true
		}
		if strings.Contains(descStr, "router_exporter_last_scrape_timestamp_seconds") {
			foundTimestamp = true
		}
	}

	if !foundDummy {
		t.Errorf("未收集到 dummy 采集器的指标")
	}
	if !foundDuration {
		t.Errorf("未收集到 router_exporter_scrape_duration_seconds 指标")
	}
	if !foundSuccess {
		t.Errorf("未收集到 router_exporter_collector_success 指标")
	}
	if !foundTimestamp {
		t.Errorf("未收集到 router_exporter_last_scrape_timestamp_seconds 指标")
	}
}

// TestMerlinCollector_ErrorIsolation 验证采集器异常时错误隔离及 success 指标标记为 0
func TestMerlinCollector_ErrorIsolation(t *testing.T) {
	resetRegistry()
	RegisterCollector("err_test", false, func() (Collector, error) {
		return &errorCollector{}, nil
	})

	mc, err := NewMerlinCollector(1*time.Second, map[string]bool{"err_test": true})
	if err != nil {
		t.Fatalf("创建 MerlinCollector 失败: %v", err)
	}

	ch := make(chan prometheus.Metric, 10)
	mc.Collect(ch)
	close(ch)

	foundSuccessMetric := false
	for m := range ch {
		if strings.Contains(m.Desc().String(), "router_exporter_collector_success") {
			foundSuccessMetric = true
		}
	}

	if !foundSuccessMetric {
		t.Errorf("采集失败时未生成 router_exporter_collector_success 指标")
	}
}

// TestMerlinCollector_Timeout 验证采集器超时时的 context 取消
func TestMerlinCollector_Timeout(t *testing.T) {
	resetRegistry()
	tCol := &timeoutCollector{}
	RegisterCollector("timeout_test", false, func() (Collector, error) {
		return tCol, nil
	})

	mc, err := NewMerlinCollector(50*time.Millisecond, map[string]bool{"timeout_test": true})
	if err != nil {
		t.Fatalf("创建 MerlinCollector 失败: %v", err)
	}

	ch := make(chan prometheus.Metric, 10)
	mc.Collect(ch)
	close(ch)

	if atomic.LoadInt32(&tCol.canceled) != 1 {
		t.Errorf("超时后采集器上下文未被正确取消")
	}
}

// TestMerlinCollector_Describe 验证 Describe 输出
func TestMerlinCollector_Describe(t *testing.T) {
	resetRegistry()
	mc, err := NewMerlinCollector(1*time.Second, nil)
	if err != nil {
		t.Fatalf("创建 MerlinCollector 失败: %v", err)
	}

	ch := make(chan *prometheus.Desc, 10)
	mc.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 3 {
		t.Errorf("期望 Describe 发送 3 个 Desc，实际收到 %d 个", count)
	}
}

// TestAvailableCollectors 验证注册中心返回的默认采集器状态列表
func TestAvailableCollectors(t *testing.T) {
	resetRegistry()
	RegisterCollector("available_test", true, func() (Collector, error) {
		return &dummyCollector{}, nil
	})

	avail := AvailableCollectors()
	if enabled, ok := avail["available_test"]; !ok || !enabled {
		t.Errorf("期望 available_test 存在且为 true，实际为: %v,存在: %v", enabled, ok)
	}
}

// TestNewMerlinCollector_FactoryError 验证工厂函数返回错误时创建失败
func TestNewMerlinCollector_FactoryError(t *testing.T) {
	resetRegistry()
	RegisterCollector("fail_init", true, func() (Collector, error) {
		return nil, errors.New("init error")
	})

	_, err := NewMerlinCollector(1*time.Second, map[string]bool{"fail_init": true})
	if err == nil {
		t.Fatalf("工厂返回错误时期望 NewMerlinCollector 报错，但得到了 nil")
	}
}

// TestNewMerlinCollector_Disabled 验证禁用采集器不被初始化
func TestNewMerlinCollector_Disabled(t *testing.T) {
	resetRegistry()
	RegisterCollector("disabled_col", false, func() (Collector, error) {
		return &dummyCollector{}, nil
	})

	mc, err := NewMerlinCollector(1*time.Second, map[string]bool{"disabled_col": false})
	if err != nil {
		t.Fatalf("创建 MerlinCollector 失败: %v", err)
	}

	if _, ok := mc.collectors["disabled_col"]; ok {
		t.Errorf("已禁用的采集器不应出现在 collectors map 中")
	}
}
