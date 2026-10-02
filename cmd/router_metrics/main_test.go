package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metrics/collector"

	"github.com/prometheus/client_golang/prometheus"
)

// TestParseFlags_Defaults 验证未提供参数时的默认参数解析
func TestParseFlags_Defaults(t *testing.T) {
	// 保存并恢复全局变量状态
	origTrafficFlag := collector.LANClientTrafficFlag
	defer func() {
		collector.LANClientTrafficFlag = origTrafficFlag
	}()

	var buf bytes.Buffer
	cfg, err := parseFlags([]string{}, &buf)
	if err != nil {
		t.Fatalf("预期解析成功，但得到错误: %v", err)
	}

	if cfg.ListenAddress != ":9101" {
		t.Errorf("预期默认 ListenAddress 为 :9101，实际为: %s", cfg.ListenAddress)
	}
	if cfg.TelemetryPath != "/metrics" {
		t.Errorf("预期默认 TelemetryPath 为 /metrics，实际为: %s", cfg.TelemetryPath)
	}
	if cfg.ScrapeTimeout != 5*time.Second {
		t.Errorf("预期默认 ScrapeTimeout 为 5s，实际为: %v", cfg.ScrapeTimeout)
	}
	if cfg.ShowVersion {
		t.Errorf("预期默认 ShowVersion 为 false，实际为: %t", cfg.ShowVersion)
	}
	if collector.LANClientTrafficFlag {
		t.Errorf("预期默认 collector.LANClientTrafficFlag 为 false，实际为: %t", collector.LANClientTrafficFlag)
	}

	for name, defaultVal := range collector.AvailableCollectors() {
		if enabled, exists := cfg.EnabledCollectors[name]; !exists || enabled != defaultVal {
			t.Errorf("采集器 %s 的状态不符合预期: exists=%t, enabled=%t, default=%t", name, exists, enabled, defaultVal)
		}
	}
}

// TestParseFlags_Custom 验证自定义命令行参数覆盖
func TestParseFlags_Custom(t *testing.T) {
	origTrafficFlag := collector.LANClientTrafficFlag
	defer func() {
		collector.LANClientTrafficFlag = origTrafficFlag
	}()

	args := []string{
		"--web.listen-address=:9999",
		"--web.telemetry-path=/custom_metrics",
		"--scrape.timeout=10s",
		"--version",
		"--collector.system=false",
		"--collector.lan-client.traffic=true",
	}

	var buf bytes.Buffer
	cfg, err := parseFlags(args, &buf)
	if err != nil {
		t.Fatalf("自定义参数解析失败: %v", err)
	}

	if cfg.ListenAddress != ":9999" {
		t.Errorf("ListenAddress 未生效，实际为: %s", cfg.ListenAddress)
	}
	if cfg.TelemetryPath != "/custom_metrics" {
		t.Errorf("TelemetryPath 未生效，实际为: %s", cfg.TelemetryPath)
	}
	if cfg.ScrapeTimeout != 10*time.Second {
		t.Errorf("ScrapeTimeout 未生效，实际为: %v", cfg.ScrapeTimeout)
	}
	if !cfg.ShowVersion {
		t.Errorf("ShowVersion 预期为 true，实际为: %t", cfg.ShowVersion)
	}
	if !collector.LANClientTrafficFlag {
		t.Errorf("collector.LANClientTrafficFlag 预期为 true，实际为: %t", collector.LANClientTrafficFlag)
	}
	if cfg.EnabledCollectors["system"] != false {
		t.Errorf("collector.system 预期为 false，实际为: %t", cfg.EnabledCollectors["system"])
	}
}

// TestParseFlags_Help 验证帮助信息输出与所有采集器参数绑定
func TestParseFlags_Help(t *testing.T) {
	var buf bytes.Buffer
	_, err := parseFlags([]string{"--help"}, &buf)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("预期返回 flag.ErrHelp，实际返回: %v", err)
	}

	output := buf.String()
	requiredFlags := []string{
		"-web.listen-address",
		"-web.telemetry-path",
		"-scrape.timeout",
		"-version",
		"-collector.lan-client.traffic",
	}

	for _, rf := range requiredFlags {
		if !strings.Contains(output, rf) {
			t.Errorf("帮助信息中缺失关键 Flag: %s\n完整输出:\n%s", rf, output)
		}
	}

	for name := range collector.AvailableCollectors() {
		colFlag := "-collector." + name
		if !strings.Contains(output, colFlag) {
			t.Errorf("帮助信息中缺失采集器 Flag: %s", colFlag)
		}
	}
}

// TestParseFlags_Invalid 验证传入未知参数时返回解析错误
func TestParseFlags_Invalid(t *testing.T) {
	var buf bytes.Buffer
	_, err := parseFlags([]string{"--invalid-flag-nonexistent"}, &buf)
	if err == nil {
		t.Fatalf("预期解析非法参数报错，但未返回错误")
	}
}

// TestPrintVersion 验证版本信息格式正确
func TestPrintVersion(t *testing.T) {
	var buf bytes.Buffer
	printVersion(&buf)
	out := buf.String()

	if !strings.Contains(out, Version) {
		t.Errorf("版本输出中缺失 Version (%s): %s", Version, out)
	}
	if !strings.Contains(out, Commit) {
		t.Errorf("版本输出中缺失 Commit (%s): %s", Commit, out)
	}
	if !strings.Contains(out, BuildTime) {
		t.Errorf("版本输出中缺失 BuildTime (%s): %s", BuildTime, out)
	}
}

// TestSetupRouter 验证 HTTP 端点路由及响应
func TestSetupRouter(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := &Config{
		ListenAddress:     ":9101",
		TelemetryPath:     "/metrics",
		ScrapeTimeout:     5 * time.Second,
		EnabledCollectors: map[string]bool{},
	}

	handler := setupRouter(cfg, reg)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 1. 测试 /healthz
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("请求 /healthz 失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz 预期状态码 200，实际为: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK\n" {
		t.Errorf("/healthz 预期返回 'OK\\n'，实际为: %q", string(body))
	}

	// 2. 测试 / (根路径首页)
	respRoot, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("请求 / 失败: %v", err)
	}
	defer respRoot.Body.Close()
	if respRoot.StatusCode != http.StatusOK {
		t.Errorf("/ 预期状态码 200，实际为: %d", respRoot.StatusCode)
	}
	rootBody, _ := io.ReadAll(respRoot.Body)
	if !strings.Contains(string(rootBody), "Merlin Exporter") {
		t.Errorf("/ 页面缺失标题: %s", string(rootBody))
	}
	if !strings.Contains(string(rootBody), `href="/metrics"`) {
		t.Errorf("/ 页面缺失 metrics 链接: %s", string(rootBody))
	}

	// 3. 测试 404
	resp404, err := http.Get(ts.URL + "/not_found_endpoint")
	if err != nil {
		t.Fatalf("请求不存在路径失败: %v", err)
	}
	defer resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("预期 404 状态码，实际为: %d", resp404.StatusCode)
	}

	// 4. 测试 /metrics
	respMetrics, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer respMetrics.Body.Close()
	if respMetrics.StatusCode != http.StatusOK {
		t.Errorf("/metrics 预期状态码 200，实际为: %d", respMetrics.StatusCode)
	}
}

// TestSetupRouter_RootTelemetryPath 验证当 TelemetryPath 为根路径时能够正常访问指标
func TestSetupRouter_RootTelemetryPath(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := &Config{
		ListenAddress:     ":9101",
		TelemetryPath:     "/",
		ScrapeTimeout:     5 * time.Second,
		EnabledCollectors: map[string]bool{},
	}

	handler := setupRouter(cfg, reg)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("请求 / 失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("TelemetryPath 为 / 时预期状态码 200，实际为: %d", resp.StatusCode)
	}
}

// TestRunServer_GracefulShutdown 验证优雅停机逻辑
func TestRunServer_GracefulShutdown(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := &Config{
		ListenAddress:     "127.0.0.1:0", // 使用随机可用端口
		TelemetryPath:     "/metrics",
		ScrapeTimeout:     1 * time.Second,
		EnabledCollectors: map[string]bool{},
	}

	server := &http.Server{
		Addr:    cfg.ListenAddress,
		Handler: setupRouter(cfg, reg),
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(ctx, server)
	}()

	// 稍作等待确保服务启动监听
	time.Sleep(50 * time.Millisecond)

	// 触发优雅停机
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runServer 优雅停机发生错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("runServer 优雅停机超时")
	}
}
