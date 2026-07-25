package userdef

import (
	"fmt"
	"strconv"
	"strings"

	"agentgo/internal/trace"
)

// when 条件求值（§6.1.7）。
//
// 叶子表达式（字符串形式）：`<operand> <op> <operand>`
//
//   operand: ${event.x.y} 或字面量（数字 / "字符串" / 'string' / bareword）
//   op:      ==  !=  <  <=  >  >=  in
//   in:      右侧必须是方括号列表 [a, b, c]，元素可以是字面量或 ${...}
//
// 逻辑组合（map 形式，可嵌套）：
//
//   when:
//     and:                        # 全部子条件成立才为真
//       - "${event.task.retry_count} >= 2"
//       - or:                     # 任一子条件成立即为真
//           - "${event.agent.id} == worker-1"
//           - "${event.agent.id} == worker-2"
//       - not: "${event.task.id} == T-0"   # 单条件取反
//
// 组合规则：
//   - and / or 的值必须是非空条件列表；not 的值是单个条件（叶子或下一层组合）
//   - 每个组合 map 恰好包含一个键（and/or/not 之一）
//   - 组合嵌套最深 maxWhenNesting 层，防止病态配置
//   - 叶子字符串内仍不支持行内 and/or/not/&&/||（引号与 ${} 外的裸词会被拒绝），
//     需要组合时请使用上述嵌套结构
//
// 类型语义：
//   - 比较运算符（< <= > >=）：左右两侧解析后都尝试转 int；
//     任一失败则按字符串比较（lexical），保持可预测。
//   - 等值运算符（== !=）：永远字符串比较，避免 "5" == 5 类的踩坑。
//   - in：成员关系，字符串相等比对。

// maxWhenNesting 是 and/or/not 组合节点的最大嵌套层数（叶子不计层）。
const maxWhenNesting = 8

// whenCond 是已解析的 when 条件树；nil 表示无条件（恒真）。
//
// logic 为空时是叶子比较节点（left/op/right 有效）；
// logic 为 "and"/"or"/"not" 时是组合节点（children 为子条件，not 恰好一个）。
type whenCond struct {
	logic    string
	children []*whenCond

	left  operand
	op    string
	right []operand // 单元素：普通运算；多元素：in 列表
}

type operand struct {
	isVar bool
	raw   string // isVar=true 时是 path（不含 ${}），false 时是字面量原文
}

// parseWhen 解析 YAML `when:` 字段的原始值。
//
// nil 或空字符串返回 nil（恒真）。字符串按叶子表达式解析；
// map[string]any 按 and/or/not 逻辑组合解析（可嵌套，最深 maxWhenNesting 层）。
func parseWhen(raw any) (*whenCond, error) {
	if raw == nil {
		return nil, nil
	}
	if s, ok := raw.(string); ok && strings.TrimSpace(s) == "" {
		return nil, nil
	}
	return parseWhenNode(raw, "when", 0)
}

// parseWhenNode 递归解析条件节点；path 用于中文报错定位（如 when.or[2]）。
// depth 是已进入的组合节点层数（顶层为 0）。
func parseWhenNode(raw any, path string, depth int) (*whenCond, error) {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("%s: 条件表达式不能为空", path)
		}
		return parseWhenLeaf(v, path)
	case map[string]any:
		if depth >= maxWhenNesting {
			return nil, fmt.Errorf("%s: and/or/not 组合嵌套超过最大深度 %d 层", path, maxWhenNesting)
		}
		if len(v) != 1 {
			return nil, fmt.Errorf("%s: 组合节点必须恰好包含一个键（and/or/not），实际 %d 个", path, len(v))
		}
		for key, val := range v {
			switch key {
			case "and", "or":
				list, ok := val.([]any)
				if !ok {
					return nil, fmt.Errorf("%s.%s: %s 需要非空条件列表，实际类型 %T", path, key, key, val)
				}
				if len(list) == 0 {
					return nil, fmt.Errorf("%s.%s: 子条件列表不能为空", path, key)
				}
				children := make([]*whenCond, 0, len(list))
				for i, item := range list {
					child, err := parseWhenNode(item, fmt.Sprintf("%s.%s[%d]", path, key, i), depth+1)
					if err != nil {
						return nil, err
					}
					children = append(children, child)
				}
				return &whenCond{logic: key, children: children}, nil
			case "not":
				if _, isList := val.([]any); isList {
					return nil, fmt.Errorf("%s.not: not 只接受单个条件，不支持列表", path)
				}
				child, err := parseWhenNode(val, path+".not", depth+1)
				if err != nil {
					return nil, err
				}
				return &whenCond{logic: "not", children: []*whenCond{child}}, nil
			default:
				return nil, fmt.Errorf("%s: 未知的组合键 %q（仅支持 and/or/not）", path, key)
			}
		}
	}
	return nil, fmt.Errorf("%s: 不支持的条件类型 %T（期望字符串表达式或 and/or/not 组合）", path, raw)
}

// parseWhenLeaf 解析单行叶子表达式。空字符串由上层拦截，此处不会收到。
func parseWhenLeaf(expr string, path string) (*whenCond, error) {
	expr = strings.TrimSpace(expr)
	if hasLogicalComposition(expr) {
		return nil, fmt.Errorf("%s: 单行表达式不支持 and/or/not 逻辑组合，请改用嵌套 and:/or:/not: 结构：%q", path, expr)
	}

	// 顺序匹配：长 op 优先（防止 "<=" 被切成 "<"）
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">", " in ", " IN "} {
		idx := findOperator(expr, op)
		if idx < 0 {
			continue
		}
		opNorm := strings.TrimSpace(op)
		if strings.EqualFold(opNorm, "in") {
			opNorm = "in"
		}
		left := strings.TrimSpace(expr[:idx])
		right := strings.TrimSpace(expr[idx+len(op):])
		l, err := parseOperand(left)
		if err != nil {
			return nil, fmt.Errorf("%s: 左操作数无效：%w", path, err)
		}
		if opNorm == "in" {
			rs, err := parseList(right)
			if err != nil {
				return nil, fmt.Errorf("%s: 右操作数无效：%w", path, err)
			}
			return &whenCond{left: l, op: "in", right: rs}, nil
		}
		r, err := parseOperand(right)
		if err != nil {
			return nil, fmt.Errorf("%s: 右操作数无效：%w", path, err)
		}
		return &whenCond{left: l, op: opNorm, right: []operand{r}}, nil
	}
	return nil, fmt.Errorf("%s: 未识别运算符（支持 == != < <= > >= in）：%q", path, expr)
}

// hasLogicalComposition 拒绝叶子字符串中引号与 ${...} 之外的 and/or/not 裸词
// 及 &&/||。逻辑组合必须改用嵌套 and:/or:/not: map 结构（见 parseWhenNode），
// 防止行内写法与嵌套写法两套语义并存。
func hasLogicalComposition(expr string) bool {
	inSingle, inDouble, inVar := false, false, 0
	var token strings.Builder
	flush := func() bool {
		if token.Len() == 0 {
			return false
		}
		t := strings.ToLower(token.String())
		token.Reset()
		return t == "and" || t == "or" || t == "not"
	}
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if inVar > 0 {
			switch c {
			case '{':
				inVar++
			case '}':
				inVar--
			}
			continue
		}
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			continue
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			continue
		}
		if inSingle || inDouble {
			continue
		}
		if c == '$' && i+1 < len(expr) && expr[i+1] == '{' {
			if flush() {
				return true
			}
			inVar = 1
			i++
			continue
		}
		if (c == '&' && i+1 < len(expr) && expr[i+1] == '&') ||
			(c == '|' && i+1 < len(expr) && expr[i+1] == '|') {
			return true
		}
		if isIdentByte(c) {
			token.WriteByte(c)
			continue
		}
		if flush() {
			return true
		}
	}
	return flush()
}

func isIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

// findOperator 在表达式中查找运算符位置，**跳过引号内的内容**。
// 这样 `"foo == bar" == "x"` 不会把第一个 == 当作运算符。
func findOperator(expr, op string) int {
	inSingle, inDouble := false, false
	for i := 0; i+len(op) <= len(expr); i++ {
		c := expr[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			continue
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			continue
		}
		if inSingle || inDouble {
			continue
		}
		if expr[i:i+len(op)] == op {
			return i
		}
	}
	return -1
}

func parseOperand(s string) (operand, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return operand{}, fmt.Errorf("empty operand")
	}
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		path := s[2 : len(s)-1]
		var dummy trace.Event
		if _, ok := resolveField(dummy, path); !ok {
			return operand{}, fmt.Errorf("unknown variable reference %q", path)
		}
		return operand{isVar: true, raw: path}, nil
	}
	// 防止用户写 `event.task.depth < 5`（漏掉 ${}），parser 否则会把整段当字面字符串
	// 做 lexical 比较，永远不命中预期。命中"裸 event.x"形态时启动期硬报错。
	if strings.HasPrefix(s, "event.") {
		return operand{}, fmt.Errorf("operand %q looks like an event field reference but is missing ${} wrapping; write ${%s} instead", s, s)
	}
	return operand{isVar: false, raw: s}, nil
}

// parseList 解析 "[a, b, c]" 形式的列表，元素可以是字面量或 ${...}
func parseList(s string) ([]operand, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("'in' right side must be [list], got %q", s)
	}
	inner := s[1 : len(s)-1]
	if strings.TrimSpace(inner) == "" {
		return nil, fmt.Errorf("empty list")
	}
	parts := splitListComma(inner)
	out := make([]operand, 0, len(parts))
	for _, p := range parts {
		op, err := parseOperand(p)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

// splitListComma 按逗号切分，但忽略引号内 + ${} 内的逗号。
func splitListComma(s string) []string {
	var parts []string
	var cur strings.Builder
	inSingle, inDouble, depth := false, false, 0
	for _, c := range s {
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '{':
			depth++
		case '}':
			depth--
		}
		if c == ',' && !inSingle && !inDouble && depth == 0 {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(c)
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// resolveOperand 把 operand 求值到字符串。变量按 ev 取值，字面量去掉引号。
func resolveOperand(op operand, ev trace.Event) string {
	if op.isVar {
		v, _ := resolveField(ev, op.raw)
		return v
	}
	if v, _ := trimQuotes(op.raw); v != op.raw {
		return v
	}
	return strings.TrimSpace(op.raw)
}

// eval 对给定 trace.Event 求值。nil 接收者视为恒真——loader 把空 when 解为 nil。
func (w *whenCond) eval(ev trace.Event) bool {
	if w == nil {
		return true
	}
	switch w.logic {
	case "and": // 全部子条件成立才为真（解析期已保证列表非空）
		for _, c := range w.children {
			if !c.eval(ev) {
				return false
			}
		}
		return true
	case "or": // 任一子条件成立即为真
		for _, c := range w.children {
			if c.eval(ev) {
				return true
			}
		}
		return false
	case "not": // 单条件取反
		if len(w.children) != 1 {
			return false // 防御：解析期已保证恰好一个子条件
		}
		return !w.children[0].eval(ev)
	}
	// 叶子比较节点
	left := resolveOperand(w.left, ev)
	switch w.op {
	case "==":
		return left == resolveOperand(w.right[0], ev)
	case "!=":
		return left != resolveOperand(w.right[0], ev)
	case "<", "<=", ">", ">=":
		return compareOrdered(left, resolveOperand(w.right[0], ev), w.op)
	case "in":
		for _, r := range w.right {
			if left == resolveOperand(r, ev) {
				return true
			}
		}
		return false
	}
	return false
}

// compareOrdered 数字优先：两侧都解析为 int 时按数字比较；任一失败回落字符串字典序。
func compareOrdered(l, r, op string) bool {
	li, lerr := strconv.ParseInt(l, 10, 64)
	ri, rerr := strconv.ParseInt(r, 10, 64)
	if lerr == nil && rerr == nil {
		switch op {
		case "<":
			return li < ri
		case "<=":
			return li <= ri
		case ">":
			return li > ri
		case ">=":
			return li >= ri
		}
	}
	switch op {
	case "<":
		return l < r
	case "<=":
		return l <= r
	case ">":
		return l > r
	case ">=":
		return l >= r
	}
	return false
}
