package runcontract

import "testing"

func TestFrameworkBudgetProfilesSeparateTotalRunFromNoProgress(t *testing.T) {
	swe, ok := FrameworkBudgetProfile("swe/v1")
	if !ok || swe.ModelCalls != 64 || swe.ToolActions != 128 || swe.Attempts != 6 || swe.Validate() != nil {
		t.Fatalf("swe/v1 Run 总预算错误: %+v ok=%v", swe, ok)
	}
	interactive, ok := FrameworkBudgetProfile("interactive/v1")
	if !ok || interactive.ModelCalls <= swe.ModelCalls || interactive.Attempts != 6 || interactive.Validate() != nil {
		t.Fatalf("interactive/v1 Run 总预算错误: %+v ok=%v", interactive, ok)
	}
	sweV2, ok := FrameworkBudgetProfile("swe/v2")
	if !ok || sweV2.PromptTokens != 0 || sweV2.CompletionTokens != 0 || sweV2.ModelCalls != 64 {
		t.Fatalf("swe/v2 token 必须 observe-only 且保留调用护栏: %+v", sweV2)
	}
	interactiveV2, ok := FrameworkBudgetProfile("interactive/v2")
	if !ok || interactiveV2.PromptTokens != 0 || interactiveV2.CompletionTokens != 0 || interactiveV2.ModelCalls != 128 {
		t.Fatalf("interactive/v2 token 必须 observe-only: %+v", interactiveV2)
	}
	if _, ok := FrameworkBudgetProfile("custom/v1"); ok {
		t.Fatal("未知 profile 不得被 framework 猜测")
	}
	sweV3, ok := FrameworkActivationBudgetProfile("swe/v3")
	if !ok || sweV3.ModelCalls != 0 || sweV3.ToolActions != 0 || sweV3.Attempts != 6 || sweV3.WallTime == 0 {
		t.Fatalf("swe/v3 不得收紧正常 Activation 的调用/工具额度: %+v ok=%v", sweV3, ok)
	}
	interactiveV3, ok := FrameworkActivationBudgetProfile("interactive/v3")
	if !ok || interactiveV3.ModelCalls != 0 || interactiveV3.ToolActions != 0 || interactiveV3.Attempts != 6 {
		t.Fatalf("interactive/v3 不得收紧正常 Activation: %+v ok=%v", interactiveV3, ok)
	}
}
