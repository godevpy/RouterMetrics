package nvram

import (
	"context"
	"testing"
	"time"
)

func TestMockNVRAMClient(t *testing.T) {
	mockData := map[string]string{
		"productid": "RT-AX86U",
		"buildno":   "3004.388.7",
	}
	client := NewMockClient(mockData)

	val, err := client.Get(context.Background(), "productid")
	if err != nil || val != "RT-AX86U" {
		t.Fatalf("expected RT-AX86U, got %s (err: %v)", val, err)
	}

	all, err := client.GetAll(context.Background(), []string{"productid", "buildno", "unknown"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if all["productid"] != "RT-AX86U" || all["buildno"] != "3004.388.7" || all["unknown"] != "" {
		t.Fatalf("unexpected map: %v", all)
	}
}

func TestNewClient(t *testing.T) {
	client := NewClient(0)
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	clientCustom := NewClient(2 * time.Second)
	if clientCustom == nil {
		t.Fatal("expected non-nil client")
	}
}
