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
// ResolveModes 回落 immediate / normal / team。
func TestValidate_ModesEmptyDefaults(t *testing.T) {
	cfg := minimalValidConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败（modes 缺省应合法）: %v", err)
	}
	gate, exec, topo := cfg.ResolveModes()
	if gate != modes.GateImmediate || exec != modes.ExecNormal || topo != modes.TopoTeam {
		t.Fatalf("ResolveModes 默认值 = %v/%v/%v，期望 immediate/normal/team", gate, exec, topo)
	}
}

// TestValidate_ModesValidValues 验证三轴全部合法取值（含大小写混合）通过校验，
// 且 ResolveModes 解析到对应枚举。
func TestValidate_ModesValidValues(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Modes = ModesConfig{Gate: "Plan", Exec: "READONLY", Topo: "solo"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败（合法 modes 应通过）: %v", err)
	}
	gate, exec, topo := cfg.ResolveModes()
	if gate != modes.GatePlan || exec != modes.ExecReadonly || topo != modes.TopoSolo {
		t.Fatalf("ResolveModes = %v/%v/%v，期望 plan/readonly/solo", gate, exec, topo)
	}
}

// TestValidate_ModesPartialValues 验证只声明部分轴时，未声明轴回落默认。
func TestValidate_ModesPartialValues(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Modes = ModesConfig{Exec: "strict"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	gate, exec, topo := cfg.ResolveModes()
	if gate != modes.GateImmediate || exec != modes.ExecStrict || topo != modes.TopoTeam {
		t.Fatalf("ResolveModes = %v/%v/%v，期望 immediate/strict/team", gate, exec, topo)
	}
}

// TestValidate_ModesInvalidValues 验证非法值逐轴报错，且错误消息指明字段名。
func TestValidate_ModesInvalidValues(t *testing.T) {
	cases := []struct {
		name    string
		modes   ModesConfig
		wantSub string
	}{
		{"gate 非法", ModesConfig{Gate: "fast"}, "modes.gate"},
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
