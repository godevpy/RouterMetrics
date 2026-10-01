package util

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// RunCommandWithTimeout 在指定超时内执行系统命令，防止底层驱动或 CLI 挂起
func RunCommandWithTimeout(parentCtx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("command %s %v timed out after %v", name, args, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("command %s %v failed: %w, stderr: %s", name, args, err, stderr.String())
	}

	return stdout.Bytes(), nil
}
