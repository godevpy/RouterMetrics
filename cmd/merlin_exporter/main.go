// Package main 是 merlin_exporter 的主程序入口，负责命令行参数解析、采集器装载、HTTP 服务启动及优雅停机。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"metrics/collector"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Version 应用程序版本号 (可通过编译选项 -ldflags "-X main.Version=..." 覆盖)
	Version = "1.0.0"
	// Commit 代码 Git 提交哈希 (可通过编译选项 -ldflags "-X main.Commit=..." 覆盖)
	Commit = "none"
	// BuildTime 二进制构建时间 (可通过编译选项 -ldflags "-X main.BuildTime=..." 覆盖)
	BuildTime = "unknown"
)

// Config 封装应用程序命令行解析后产生的运行配置
type Config struct {
	ListenAddress     string
	TelemetryPath     string
	ScrapeTimeout     time.Duration
	ShowVersion       bool
	EnabledCollectors map[string]bool
}

// printVersion 向指定输出流打印版本信息
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "merlin_exporter version %s (commit: %s, build time: %s)\n", Version, Commit, BuildTime)
}

// parseFlags 解析传入的命令行参数并返回运行配置
func parseFlags(args []string, output io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("merlin_exporter", flag.ContinueOnError)
	fs.SetOutput(output)

	fs.Usage = func() {
		fmt.Fprintf(output, "Usage of %s:\n", fs.Name())
		fs.PrintDefaults()
	}

	cfg := &Config{
		EnabledCollectors: make(map[string]bool),
	}

	fs.StringVar(&cfg.ListenAddress, "web.listen-address", ":9101", "HTTP 服务监听地址")
	fs.StringVar(&cfg.TelemetryPath, "web.telemetry-path", "/metrics", "暴露 Prometheus 指标的 URL 路径")
	fs.DurationVar(&cfg.ScrapeTimeout, "scrape.timeout", 5*time.Second, "单次采集操作的全局超时时间")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "打印版本信息并退出")

	// 局域网终端无线流量采集专用开关 (绑定至 collector.LANClientTrafficFlag)
	fs.BoolVar(&collector.LANClientTrafficFlag, "collector.lan-client.traffic", false, "是否启用局域网终端无线流量统计 (默认关闭)")

	// 动态获取所有已注册的采集器并绑定对应的布尔 Flag
	available := collector.AvailableCollectors()
	collectorFlags := make(map[string]*bool, len(available))

	names := make([]string, 0, len(available))
	for name := range available {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		defaultState := available[name]
		collectorFlags[name] = fs.Bool("collector."+name, defaultState, fmt.Sprintf("是否启用 %s 指标采集器 (默认: %t)", name, defaultState))
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	for name, valPtr := range collectorFlags {
		cfg.EnabledCollectors[name] = *valPtr
	}

	return cfg, nil
}

// setupRouter 组装 HTTP 路由与处理器
func setupRouter(cfg *Config, reg prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()

	// 注册 Prometheus 指标暴露端点
	mux.Handle(cfg.TelemetryPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))

	// 注册健康检查探针端点
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})

	// 若指标路径不占用根路径，则在根路径提供欢迎和导航 Banner
	if cfg.TelemetryPath != "/" {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			bannerHTML := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Merlin Exporter</title>
</head>
<body>
    <h1>Merlin Exporter</h1>
    <p>Version: %s (Commit: %s, BuildTime: %s)</p>
    <p><a href="%s">Metrics</a></p>
</body>
</html>
`, Version, Commit, BuildTime, cfg.TelemetryPath)
			_, _ = w.Write([]byte(bannerHTML))
		})
	}

	return mux
}

// runServer 启动 HTTP 服务并配合外部 Context 实现优雅停机
func runServer(ctx context.Context, server *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Println("[INFO] 接收到退出信号，正在执行优雅停机...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅停机失败: %w", err)
		}
		log.Println("[INFO] HTTP 服务已成功安全停止")
		return nil
	}
}

func main() {
	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}

	if cfg.ShowVersion {
		printVersion(os.Stdout)
		os.Exit(0)
	}

	log.Printf("[INFO] 正在启动 merlin_exporter %s (commit: %s, build time: %s)", Version, Commit, BuildTime)

	// 创建专用的指标注册中心 (排除默认的 process/go 指标以严格控制内存开销)
	registry := prometheus.NewRegistry()
	mc, err := collector.NewMerlinCollector(cfg.ScrapeTimeout, cfg.EnabledCollectors)
	if err != nil {
		log.Fatalf("[ERROR] 初始化 MerlinCollector 失败: %v", err)
	}
	if err := registry.Register(mc); err != nil {
		log.Fatalf("[ERROR] 注册 MerlinCollector 失败: %v", err)
	}

	router := setupRouter(cfg, registry)
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("[INFO] HTTP 监听端口: %s，指标路径: %s", cfg.ListenAddress, cfg.TelemetryPath)
	if err := runServer(ctx, server); err != nil {
		log.Fatalf("[ERROR] 服务运行异常: %v", err)
	}
}
