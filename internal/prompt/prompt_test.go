package prompt

import (
	"strings"
	"testing"
)

// TestCompile_OrderPreserved 验证组件顺序即参数序，且 Build.Text 只拼接
// InMessage 组件正文。
func TestCompile_OrderPreserved(t *testing.T) {
	build := Compile([]Component{
		{ID: ComponentAgentRole, Version: "v1", Text: "角色。", InMessage: true},
		{ID: ComponentControlProtocol, Version: "v1", Text: "协议。", InMessage: true},
		{ID: ComponentToolGuidance, Version: "v1", Text: "read_file,write_file", InMessage: false},
		{ID: ComponentTaskObjective, Version: "v1", Text: "目标。", InMessage: true},
	})
	if len(build.Components) != 4 {
		t.Fatalf("组件数=%d，期望 4", len(build.Components))
	}
	wantOrder := []string{ComponentAgentRole, ComponentControlProtocol, ComponentToolGuidance, ComponentTaskObjective}
	for i, id := range wantOrder {
		if build.Components[i].ID != id {
			t.Fatalf("组件顺序不符：位置 %d 为 %q，期望 %q", i, build.Components[i].ID, id)
		}
	}
	if build.Text != "角色。协议。目标。" {
		t.Fatalf("Build.Text 只应拼接 InMessage 组件正文，got %q", build.Text)
	}
	if build.Digest != DigestText(build.Text) {
		t.Fatalf("Build.Digest 应为 Text 的 sha256 前 12")
	}
}

// TestCompile_DigestStable 验证同输入跨调用 digest 稳定。
func TestCompile_DigestStable(t *testing.T) {
	parts := []Component{
		{ID: ComponentAgentRole, Version: "file:abc", Text: "系统提示", InMessage: true},
		{ID: ComponentTaskObjective, Version: "v1", Text: "任务描述", InMessage: true},
	}
	b1 := Compile(parts)
	b2 := Compile(append([]Component(nil), parts...))
	if b1.ID != b2.ID {
		t.Fatalf("同输入 Build.ID 应稳定：%q vs %q", b1.ID, b2.ID)
	}
	if b1.Digest != b2.Digest {
		t.Fatalf("同输入 Build.Digest 应稳定")
	}
	for i := range b1.Components {
		if b1.Components[i].Digest != b2.Components[i].Digest {
			t.Fatalf("组件 %d digest 应稳定", i)
		}
	}
}

// TestCompile_DigestDiffers 验证任一组件正文 / 版本 / ID 变化都会改变
// Build.ID；组件顺序变化同样改变 Build.ID。
func TestCompile_DigestDiffers(t *testing.T) {
	base := []Component{
		{ID: ComponentAgentRole, Version: "v1", Text: "角色", InMessage: true},
		{ID: ComponentTaskObjective, Version: "v1", Text: "目标", InMessage: true},
	}
	b0 := Compile(base)

	// 正文变化
	b1 := Compile([]Component{
		{ID: ComponentAgentRole, Version: "v1", Text: "角色（改）", InMessage: true},
		{ID: ComponentTaskObjective, Version: "v1", Text: "目标", InMessage: true},
	})
	if b1.ID == b0.ID {
		t.Fatalf("组件正文变化应改变 Build.ID")
	}
	// 版本变化（正文不变）
	b2 := Compile([]Component{
		{ID: ComponentAgentRole, Version: "v2", Text: "角色", InMessage: true},
		{ID: ComponentTaskObjective, Version: "v1", Text: "目标", InMessage: true},
	})
	if b2.ID == b0.ID {
		t.Fatalf("组件版本变化应改变 Build.ID")
	}
	// 顺序变化
	b3 := Compile([]Component{base[1], base[0]})
	if b3.ID == b0.ID {
		t.Fatalf("组件顺序变化应改变 Build.ID")
	}
	// 带外组件变化也改变 Build.ID（但不改变 Build.Text）
	b4 := Compile(append(append([]Component(nil), base...),
		Component{ID: ComponentToolGuidance, Version: "v1", Text: "read_file", InMessage: false}))
	if b4.ID == b0.ID {
		t.Fatalf("新增带外组件应改变 Build.ID")
	}
	if b4.Text != b0.Text {
		t.Fatalf("带外组件不应进入 Build.Text")
	}
}

// TestCompile_ComponentIdentity 验证组件身份：Digest 由 Compile 统一计算
//（覆盖调用方预置值），且等于 Text 的 sha256 前 12。
func TestCompile_ComponentIdentity(t *testing.T) {
	build := Compile([]Component{
		{ID: ComponentAgentRole, Version: "v1", Digest: "调用方预置应被覆盖", Text: "正文", InMessage: true},
	})
	c := build.Components[0]
	if c.Digest == "调用方预置应被覆盖" {
		t.Fatalf("Compile 应覆盖调用方预置的 Digest")
	}
	if c.Digest != DigestText("正文") {
		t.Fatalf("组件 Digest 应为 Text 的 sha256 前 12，got %q", c.Digest)
	}
	if len(c.Digest) != 12 {
		t.Fatalf("组件 Digest 应为 12 位 hex，got %q", c.Digest)
	}
	if !strings.HasPrefix(build.ID, "pb-") {
		t.Fatalf("Build.ID 应以 pb- 前缀标识，got %q", build.ID)
	}
}

// TestCompile_EmptyAndSummary 验证空组件序列合法，且摘要 JSON 不含正文。
func TestCompile_EmptyAndSummary(t *testing.T) {
	empty := Compile(nil)
	if empty.ID == "" || empty.Text != "" {
		t.Fatalf("空编译应产出稳定 ID 且 Text 为空，got %+v", empty)
	}

	build := Compile([]Component{
		{ID: ComponentAgentRole, Version: "v1", Text: "秘密正文不得进摘要", InMessage: true},
		{ID: ComponentToolGuidance, Version: "v1", Text: "read_file", InMessage: false},
	})
	summary := build.ComponentsSummaryJSON()
	if strings.Contains(summary, "秘密正文") {
		t.Fatalf("摘要不得包含组件正文：%s", summary)
	}
	for _, want := range []string{`"id":"agent_role"`, `"id":"tool_guidance"`, `"in_message":false`, `"digest"`} {
		if !strings.Contains(summary, want) {
			t.Fatalf("摘要缺少 %q：%s", want, summary)
		}
	}
}
