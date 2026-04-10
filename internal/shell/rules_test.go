package shell

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestMergeRules_Basic(t *testing.T) {
	defaultRules := []string{`rm -rf /`, `mkfs`}
	globalCustom := []string{`dangerous_cmd`, `rm -rf /`} // 重复项
	patch := RulePatch{
		Add:    []string{`project_specific`},
		Remove: []string{`mkfs`},
	}

	result := MergeRules(defaultRules, globalCustom, patch)
	sort.Strings(result)

	expected := []string{`dangerous_cmd`, `project_specific`, `rm -rf /`}
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("MergeRules() = %v, want %v", result, expected)
	}
}

func TestMergeRules_Empty(t *testing.T) {
	// 全部为空
	result := MergeRules(nil, nil, RulePatch{})
	if len(result) != 0 {
		t.Errorf("期望空切片，得到 %v", result)
	}

	// 只有默认值
	result = MergeRules([]string{`a`, `b`}, nil, RulePatch{})
	sort.Strings(result)
	expected := []string{`a`, `b`}
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("得到 %v，期望 %v", result, expected)
	}
}

func TestMergeRules_RemoveNonExistent(t *testing.T) {
	// 尝试移除不存在的规则（不应报错）
	defaultRules := []string{`a`, `b`}
	patch := RulePatch{
		Remove: []string{`c`}, // 不存在
	}

	result := MergeRules(defaultRules, nil, patch)
	sort.Strings(result)

	expected := []string{`a`, `b`}
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("得到 %v，期望 %v", result, expected)
	}
}

func TestMergeRules_AddAndRemovePriority(t *testing.T) {
	// 测试 add 和 remove 的优先级：先 add 后 remove
	// 即使 add 添加的规则，也可以被 remove 移除
	patch := RulePatch{
		Add:    []string{`same_rule`},
		Remove: []string{`same_rule`}, // 同名规则先加后删
	}

	result := MergeRules([]string{`default`}, nil, patch)

	// same_rule 应该被移除
	expected := []string{`default`}
	sort.Strings(result)
	sort.Strings(expected)
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("add 后 remove 应生效，得到 %v，期望 %v", result, expected)
	}
}

func TestMergeRules_DuplicateRemoval(t *testing.T) {
	// 测试去重：默认和全局自定义有重复项
	defaultRules := []string{`rule1`, `rule2`}
	globalCustom := []string{`rule2`, `rule3`} // rule2 重复

	result := MergeRules(defaultRules, globalCustom, RulePatch{})
	sort.Strings(result)

	// 结果不应有重复
	expected := []string{`rule1`, `rule2`, `rule3`}
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("应去重，得到 %v，期望 %v", result, expected)
	}
}

func TestLoadProjectRules_NotExist(t *testing.T) {
	// 文件不存在时应返回空规则（不是错误）
	rules, err := LoadProjectRules("/nonexistent/path")
	if err != nil {
		t.Errorf("文件不存在不应返回错误，得到: %v", err)
	}
	if len(rules.ShellRules.Blacklist.Add) != 0 || len(rules.ShellRules.Greylist.Add) != 0 {
		t.Error("期望空规则")
	}
}

func TestLoadProjectRules_ValidFile(t *testing.T) {
	// 创建临时目录和有效规则文件
	tmpDir := t.TempDir()
	content := `
shell_rules:
  blacklist:
    add:
      - "npm\\s+publish"
      - "docker\\s+push"
    remove:
      - "shutdown"
  greylist:
    add:
      - "make\\s+deploy"
    remove:
      - "git\\s+push"
`
	rulesPath := filepath.Join(tmpDir, ".agentgo", "project_rules.yaml")
	if err := os.MkdirAll(filepath.Dir(rulesPath), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(rulesPath, []byte(content), 0644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	// 加载规则
	rules, err := LoadProjectRules(tmpDir)
	if err != nil {
		t.Fatalf("加载规则失败: %v", err)
	}

	// 验证黑名单
	if len(rules.ShellRules.Blacklist.Add) != 2 {
		t.Errorf("黑名单 add 应有 2 项，得到 %d", len(rules.ShellRules.Blacklist.Add))
	}
	if len(rules.ShellRules.Blacklist.Remove) != 1 {
		t.Errorf("黑名单 remove 应有 1 项，得到 %d", len(rules.ShellRules.Blacklist.Remove))
	}

	// 验证灰名单
	if len(rules.ShellRules.Greylist.Add) != 1 {
		t.Errorf("灰名单 add 应有 1 项，得到 %d", len(rules.ShellRules.Greylist.Add))
	}
	if len(rules.ShellRules.Greylist.Remove) != 1 {
		t.Errorf("灰名单 remove 应有 1 项，得到 %d", len(rules.ShellRules.Greylist.Remove))
	}

	// 验证具体内容
	if rules.ShellRules.Blacklist.Add[0] != `npm\s+publish` {
		t.Errorf("黑名单第 1 项内容错误: %s", rules.ShellRules.Blacklist.Add[0])
	}
}

func TestLoadProjectRules_InvalidYAML(t *testing.T) {
	// 创建临时目录和无效 YAML
	tmpDir := t.TempDir()
	rulesPath := filepath.Join(tmpDir, ".agentgo", "project_rules.yaml")
	if err := os.MkdirAll(filepath.Dir(rulesPath), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	// 写入无效 YAML
	if err := os.WriteFile(rulesPath, []byte("invalid: yaml: content: ["), 0644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	// 加载应返回错误
	_, err := LoadProjectRules(tmpDir)
	if err == nil {
		t.Error("无效 YAML 应返回错误")
	}
}

func TestBuildFilter(t *testing.T) {
	// 使用临时目录测试
	tmpDir := t.TempDir()

	// 无项目规则时
	filter, err := BuildFilter(tmpDir, []string{`global_custom`}, nil)
	if err != nil {
		t.Fatalf("BuildFilter 失败: %v", err)
	}

	// 验证过滤器工作
	action, pattern := filter.Check("global_custom")
	if action != "block" {
		t.Errorf("global_custom 应被拦截，得到 action=%s", action)
	}
	if pattern != `global_custom` {
		t.Errorf("pattern 应为 global_custom，得到 %s", pattern)
	}
}

func TestBuildFilter_WithProjectRules(t *testing.T) {
	// 创建临时目录和规则文件
	tmpDir := t.TempDir()
	content := `
shell_rules:
  blacklist:
    add:
      - "project_dangerous"
    remove:
      - "rm\\s+-rf\\s+/"
`
	rulesPath := filepath.Join(tmpDir, ".agentgo", "project_rules.yaml")
	if err := os.MkdirAll(filepath.Dir(rulesPath), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(rulesPath, []byte(content), 0644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	// 构建过滤器
	filter, err := BuildFilter(tmpDir, []string{`global_custom`}, nil)
	if err != nil {
		t.Fatalf("BuildFilter 失败: %v", err)
	}

	// 验证全局自定义规则生效
	action, _ := filter.Check("global_custom")
	if action != "block" {
		t.Errorf("全局自定义规则应生效，得到 action=%s", action)
	}

	// 验证项目级 add 生效
	action, _ = filter.Check("project_dangerous")
	if action != "block" {
		t.Errorf("项目级 add 应生效，得到 action=%s", action)
	}

	// 验证项目级 remove 生效（默认的 rm -rf / 被移除）
	action, _ = filter.Check("rm -rf /")
	if action == "block" {
		t.Error("rm -rf / 应被项目级 remove 移除，但仍被拦截")
	}
}

func TestBuildFilter_WithGreylist(t *testing.T) {
	// 创建临时目录和规则文件（含灰名单）
	tmpDir := t.TempDir()
	content := `
shell_rules:
  greylist:
    add:
      - "project_deploy"
    remove:
      - "git\\s+push"
`
	rulesPath := filepath.Join(tmpDir, ".agentgo", "project_rules.yaml")
	if err := os.MkdirAll(filepath.Dir(rulesPath), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(rulesPath, []byte(content), 0644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	// 构建过滤器（含灰名单）
	filter, err := BuildFilter(tmpDir, nil, []string{`global_grey`})
	if err != nil {
		t.Fatalf("BuildFilter 失败: %v", err)
	}

	// 验证全局灰名单生效
	action, _ := filter.Check("global_grey")
	if action != "approve" {
		t.Errorf("全局灰名单应生效，得到 action=%s", action)
	}

	// 验证项目级灰名单 add 生效
	action, _ = filter.Check("project_deploy")
	if action != "approve" {
		t.Errorf("项目级灰名单 add 应生效，得到 action=%s", action)
	}

	// 验证项目级灰名单 remove 生效（默认的 git push 被移除）
	action, _ = filter.Check("git push origin main")
	if action == "approve" {
		t.Error("git push 应被项目级 remove 移除，但仍需审批")
	}
}

func TestBuildFilter_InvalidProjectRules(t *testing.T) {
	// 创建临时目录和无效规则文件
	tmpDir := t.TempDir()
	rulesPath := filepath.Join(tmpDir, ".agentgo", "project_rules.yaml")
	if err := os.MkdirAll(filepath.Dir(rulesPath), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(rulesPath, []byte("invalid yaml content ["), 0644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	// BuildFilter 应传播错误
	_, err := BuildFilter(tmpDir, nil, nil)
	if err == nil {
		t.Error("无效项目规则应返回错误")
	}
}

func TestBuildFilter_EmptyRules(t *testing.T) {
	tmpDir := t.TempDir()

	// 全部为空
	filter, err := BuildFilter(tmpDir, nil, nil)
	if err != nil {
		t.Fatalf("BuildFilter 失败: %v", err)
	}

	// 验证使用默认规则
	action, _ := filter.Check("rm -rf /")
	if action != "block" {
		t.Errorf("应使用默认规则，rm -rf / 应被拦截，得到 action=%s", action)
	}

	action, _ = filter.Check("git push")
	if action != "approve" {
		t.Errorf("应使用默认规则，git push 应需审批，得到 action=%s", action)
	}
}
