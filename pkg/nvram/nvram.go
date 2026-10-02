package nvram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"metrics/pkg/util"
)

// Client 定义 NVRAM 读取接口
type Client interface {
	Get(ctx context.Context, key string) (string, error)
	GetAll(ctx context.Context, keys []string) (map[string]string, error)
}

type realClient struct {
	timeout time.Duration
}

// NewClient 创建基于系统 nvram 命令的实际 Client 实例
func NewClient(timeout time.Duration) Client {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &realClient{timeout: timeout}
}

func (c *realClient) Get(ctx context.Context, key string) (string, error) {
	out, err := util.RunCommandWithTimeout(ctx, c.timeout, "nvram", "get", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *realClient) GetAll(ctx context.Context, keys []string) (map[string]string, error) {
	res := make(map[string]string, len(keys))
	var firstErr error
	failures := 0
	for _, k := range keys {
		val, err := c.Get(ctx, k)
		if err != nil {
			// 记录首个错误并继续读取其余变量，避免单个 key 失败导致全部中断
			if firstErr == nil {
				firstErr = err
			}
			failures++
			res[k] = ""
			continue
		}
		res[k] = val
	}
	// 全部 key 均读取失败说明 nvram 命令本身不可用（缺失、无权限等），
	// 必须向调用方返回错误，避免静默上报虚假指标
	if len(keys) > 0 && failures == len(keys) {
		return res, fmt.Errorf("全部 %d 个 NVRAM 变量读取失败: %w", failures, firstErr)
	}
	return res, nil
}

type mockClient struct {
	data map[string]string
}

// NewMockClient 创建用于单元测试或模拟环境的 Mock Client 实例
func NewMockClient(data map[string]string) Client {
	return &mockClient{data: data}
}

func (m *mockClient) Get(ctx context.Context, key string) (string, error) {
	return m.data[key], nil
}

func (m *mockClient) GetAll(ctx context.Context, keys []string) (map[string]string, error) {
	res := make(map[string]string, len(keys))
	for _, k := range keys {
		res[k] = m.data[k]
	}
	return res, nil
}
