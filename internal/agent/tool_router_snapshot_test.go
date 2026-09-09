package agent

import (
	"context"
	"testing"
)

func TestFreezeToolRouterSnapshotBindsVisibleAndRuntimeView(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("read_file", "读取", map[string]any{
		"type": "object",
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })

	snapshot, err := FreezeToolRouterSnapshot(registry)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID == "" || snapshot.Registry != registry || len(snapshot.Defs) != 1 {
		t.Fatalf("ToolRouterSnapshot 不完整: %+v", snapshot)
	}

	// Defs 是值拷贝；调用方修改切片不能污染 Registry 的 model-visible authority。
	snapshot.Defs[0].Name = "tampered"
	if got := registry.Defs()[0].Name; got != "read_file" {
		t.Fatalf("修改 snapshot defs 污染 Registry: %q", got)
	}
}
