package shell

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectRules 项目级规则覆盖结构
type ProjectRules struct {
	ShellRules ShellRulePatch `yaml:"shell_rules"`
}

// ShellRulePatch Shell 规则补丁
type ShellRulePatch struct {
	Blacklist RulePatch `yaml:"blacklist"` // 黑名单补丁
	Greylist  RulePatch `yaml:"greylist"`  // 灰名单补丁
}

// RulePatch 规则补丁（add=追加, remove=移除）
type RulePatch struct {
	Add    []string `yaml:"add"`    // 要追加的规则
	Remove []string `yaml:"remove"` // 要移除的规则
}

// LoadProjectRules 从 .agentgo/project_rules.yaml 加载项目级规则
// 文件不存在时返回空规则（不是错误）
func LoadProjectRules(projectRoot string) (*ProjectRules, error) {
	path := filepath.Join(projectRoot, ".agentgo", "project_rules.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectRules{}, nil
		}
		return nil, fmt.Errorf("读取项目规则失败: %w", err)
	}

	var rules ProjectRules
	if err := yaml.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("解析项目规则失败: %w", err)
	}

	return &rules, nil
}

// MergeRules 合并默认规则 + 全局自定义 + 项目级补丁
// 优先级：默认规则 < 全局自定义 < 项目级 add < 项目级 remove
func MergeRules(defaultRules, globalCustom []string, patch RulePatch) []string {
	// 1. 基础：默认规则 + 全局自定义
	merged := make([]string, 0, len(defaultRules)+len(globalCustom)+len(patch.Add))
	merged = append(merged, defaultRules...)
	merged = append(merged, globalCustom...)

	// 2. 去重集合
	ruleSet := make(map[string]bool)
	for _, r := range merged {
		ruleSet[r] = true
	}

	// 3. 应用 add 补丁
	for _, r := range patch.Add {
		ruleSet[r] = true
	}

	// 4. 应用 remove 补丁
	for _, r := range patch.Remove {
		delete(ruleSet, r)
	}

	// 5. 转回切片
	result := make([]string, 0, len(ruleSet))
	for r := range ruleSet {
		result = append(result, r)
	}

	return result
}

// BuildFilter 从配置和项目规则构建 CommandFilter
// 便捷函数，封装整个规则加载和合并流程
func BuildFilter(projectRoot string, shellBlacklist, shellGreylist []string) (*CommandFilter, error) {
	// 加载项目级规则
	projectRules, err := LoadProjectRules(projectRoot)
	if err != nil {
		return nil, err
	}

	// 合并黑名单：默认 + 全局自定义 + 项目补丁
	blacklist := MergeRules(
		DefaultBlacklist,
		shellBlacklist,
		projectRules.ShellRules.Blacklist,
	)

	// 合并灰名单
	greylist := MergeRules(
		DefaultGreylist,
		shellGreylist,
		projectRules.ShellRules.Greylist,
	)

	return NewCommandFilter(blacklist, greylist), nil
}
