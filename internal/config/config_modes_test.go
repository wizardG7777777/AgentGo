package config

import (
	"strings"
	"testing"

	"agentgo/internal/modes"
)

// minimalValidConfig 返回一个仅满足 Validate 硬性要求的最小配置
// （Scheduler-only：无静态 agents，仅需全局模型）。
func minimalValidConfig() *Config {
	cfg := DefaultConfig()
	cfg.LLM.DefaultModel = "test-model"
	return cfg
}

// TestValidate_ModesEmptyDefaults 验证 modes 块整体缺省时 Validate 通过，
// ResolveModes 回落 normal / team。
func TestValidate_ModesEmptyDefaults(t *testing.T) {
	cfg := minimalValidConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败（modes 缺省应合法）: %v", err)
	}
	exec, topo := cfg.ResolveModes()
	if exec != modes.ExecNormal || topo != modes.TopoTeam {
		t.Fatalf("ResolveModes 默认值 = %v/%v，期望 normal/team", exec, topo)
	}
}

// TestValidate_ModesValidValues 验证两轴全部合法取值（含大小写混合）通过校验，
// 且 ResolveModes 解析到对应枚举。
func TestValidate_ModesValidValues(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Modes = ModesConfig{Exec: "READONLY", Topo: "solo"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败（合法 modes 应通过）: %v", err)
	}
	exec, topo := cfg.ResolveModes()
	if exec != modes.ExecReadonly || topo != modes.TopoSolo {
		t.Fatalf("ResolveModes = %v/%v，期望 readonly/solo", exec, topo)
	}
}

// TestValidate_ModesPartialValues 验证只声明部分轴时，未声明轴回落默认。
func TestValidate_ModesPartialValues(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Modes = ModesConfig{Exec: "strict"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	exec, topo := cfg.ResolveModes()
	if exec != modes.ExecStrict || topo != modes.TopoTeam {
		t.Fatalf("ResolveModes = %v/%v，期望 strict/team", exec, topo)
	}
}

// TestValidate_ModesInvalidValues 验证非法值逐轴报错，且错误消息指明字段名。
func TestValidate_ModesInvalidValues(t *testing.T) {
	cases := []struct {
		name    string
		modes   ModesConfig
		wantSub string
	}{
		{"exec 非法", ModesConfig{Exec: "super"}, "modes.exec"},
		{"topo 非法", ModesConfig{Topo: "pair"}, "modes.topo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalValidConfig()
			cfg.Modes = tc.modes
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate 应拒绝 %+v", tc.modes)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误消息应含 %q，got: %v", tc.wantSub, err)
			}
		})
	}
}

// TestValidate_ModesGateMigrationDiagnostic 验证 V6 C6c 迁移诊断：
// gate 轴已整体移除，modes.gate 显式设为任何非空值（含曾经的合法值
// immediate 与已移除的 plan）Validate 一律拒绝，错误消息说明 gate 轴已移除、
// 执行前审阅改由 Graph approval 节点承担，并指引删除配置键。
func TestValidate_ModesGateMigrationDiagnostic(t *testing.T) {
	for _, value := range []string{"immediate", "plan", "fast"} {
		t.Run("gate="+value, func(t *testing.T) {
			cfg := minimalValidConfig()
			cfg.Modes = ModesConfig{Gate: value}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate 应拒绝 modes.gate=%q", value)
			}
			for _, want := range []string{"modes.gate", "已于 V6 整体移除", "Graph approval", "删除 gate 键"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("迁移诊断应含 %q，got: %v", want, err)
				}
			}
			// 迁移诊断下 ResolveModes 防御性回落两轴默认（该路径正常不会被
			// 走到，因为 Validate 已先行拒绝）。
			exec, topo := cfg.ResolveModes()
			if exec != modes.ExecNormal || topo != modes.TopoTeam {
				t.Fatalf("ResolveModes 应回落 normal/team，got %v/%v", exec, topo)
			}
		})
	}
}
