package workspace

import (
	"context"
	"fmt"
	"sort"
)

// ResolveCandidateBaseline 根据不可变候选的实际父链选择唯一后继版本。
// 没有模型指定的工作目录/基线槽；无关分支不能被静默覆盖或自动合并。
func (m *Manager) ResolveCandidateBaseline(ctx context.Context, graphID, runID string, refs []string) (string, error) {
	unique := map[string]bool{}
	for _, ref := range refs {
		if ref != "" {
			unique[ref] = true
		}
	}
	parents := map[string]string{}
	for ref := range unique {
		seen := map[string]bool{}
		for current := ref; current != ""; {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if seen[current] {
				return "", fmt.Errorf("候选版本谱系出现循环")
			}
			seen[current] = true
			parent, known := parents[current]
			if !known {
				candidate, _, err := m.ReadCandidate(current, graphID, runID)
				if err != nil {
					return "", fmt.Errorf("读取候选谱系: %w", err)
				}
				parent = candidate.ParentRef
				parents[current] = parent
			}
			current = parent
		}
	}
	leaves := map[string]bool{}
	for ref := range unique {
		leaves[ref] = true
	}
	for ref := range unique {
		for parent := parents[ref]; parent != ""; parent = parents[parent] {
			delete(leaves, parent)
		}
	}
	if len(leaves) > 1 {
		conflicts := make([]string, 0, len(leaves))
		for ref := range leaves {
			conflicts = append(conflicts, ref)
		}
		sort.Strings(conflicts)
		return "", fmt.Errorf("candidate_branch_conflict: 输入包含不同分支的代码候选 %v；安排整合任务形成一个后继候选，不能任取工作基线", conflicts)
	}
	for ref := range leaves {
		return ref, nil
	}
	return "", nil
}
