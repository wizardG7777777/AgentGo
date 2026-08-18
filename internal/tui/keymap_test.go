package tui

import (
	"strings"
	"testing"
)

// 键位纪律的钉板测试（2026-08 键位对齐后的防回归，与 app_test.go 的
// TestKeymap_* 互补——那边钉渲染与常量契约，这里钉禁用面）：
//  1. 无裸字母/数字动作键（误操作面，用户明确禁用）；
//  2. 无 ctrl+shift 组合（Windows 上与粘贴/输入法冲突，全部禁用）；
//  3. 无 alt/cmd 组合（alt+enter 已删，macOS 上 alt 是字符输入修饰）；
//  4. ctrl 组合只保留四个已分发键：ctrl+c / ctrl+l / ctrl+j / ctrl+v。

func keyDisplayFields(e keymapEntry) map[string]string {
	return map[string]string{"keys": e.keys, "helpKeys": e.helpKeys}
}

func TestKeymap_NoBareLetterOrDigitKeys(t *testing.T) {
	for _, e := range keymap {
		for field, v := range keyDisplayFields(e) {
			for _, tok := range strings.Fields(v) {
				if len(tok) == 1 && ((tok[0] >= 'a' && tok[0] <= 'z') || (tok[0] >= 'A' && tok[0] <= 'Z') || (tok[0] >= '0' && tok[0] <= '9')) {
					t.Fatalf("条目 %s 的 %s 含裸字母/数字键 %q（已禁用）", e.id, field, tok)
				}
			}
		}
	}
}

func TestKeymap_NoCtrlShiftOrAltCombos(t *testing.T) {
	for _, e := range keymap {
		for field, v := range keyDisplayFields(e) {
			lv := strings.ToLower(v)
			if strings.Contains(lv, "ctrl+shift") || strings.Contains(lv, "shift+ctrl") {
				t.Fatalf("条目 %s 的 %s 含 ctrl+shift 组合 %q（已禁用）", e.id, field, v)
			}
			if strings.Contains(lv, "alt+") || strings.Contains(lv, "cmd+") || strings.Contains(lv, "option+") {
				t.Fatalf("条目 %s 的 %s 含 alt/cmd 组合 %q（已禁用）", e.id, field, v)
			}
		}
	}
}

func TestKeymap_CtrlCombosAreExactlyTheDispatchedFour(t *testing.T) {
	allowed := map[string]bool{
		keyCtrlC: true, keyCtrlL: true, keyCtrlJ: true, keyCtrlV: true,
	}
	for _, e := range keymap {
		for field, v := range keyDisplayFields(e) {
			for _, tok := range strings.Fields(strings.ToLower(v)) {
				if !strings.HasPrefix(tok, "ctrl+") {
					continue
				}
				if !allowed[tok] {
					t.Fatalf("条目 %s 的 %s 出现未登记的 ctrl 组合 %q", e.id, field, tok)
				}
			}
		}
	}
}
