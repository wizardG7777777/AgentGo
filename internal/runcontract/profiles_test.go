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
	if _, ok := FrameworkBudgetProfile("custom/v1"); ok {
		t.Fatal("未知 profile 不得被 framework 猜测")
	}
}
