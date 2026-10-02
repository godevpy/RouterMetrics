package util

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunCommandWithTimeout_Success(t *testing.T) {
	ctx := context.Background()
	out, err := RunCommandWithTimeout(ctx, 1*time.Second, "echo", "hello merlin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello merlin" {
		t.Fatalf("expected 'hello merlin', got %q", string(out))
	}
}

func TestRunCommandWithTimeout_Timeout(t *testing.T) {
	ctx := context.Background()
	// sleep 2 秒但超时限制为 100 毫秒
	_, err := RunCommandWithTimeout(ctx, 100*time.Millisecond, "sleep", "2")
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
}
