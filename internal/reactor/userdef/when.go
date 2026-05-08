package userdef

import (
	"fmt"
	"strconv"
	"strings"

	"agentgo/internal/trace"
)

// when 条件求值（§6.1.7）。
//
// 语法：`<operand> <op> <operand>`
//
//   operand: ${event.x.y} 或字面量（数字 / "字符串" / 'string' / bareword）
//   op:      ==  !=  <  <=  >  >=  in
//   in:      右侧必须是方括号列表 [a, b, c]，元素可以是字面量或 ${...}
//
// **明确不支持**：逻辑组合（and / or / not）、括号、嵌套表达式。
// 复杂条件请写多个 reactor。详见 spec §6.1.7。
//
// 类型语义：
//   - 比较运算符（< <= > >=）：左右两侧解析后都尝试转 int；
//     任一失败则按字符串比较（lexical），保持可预测。
//   - 等值运算符（== !=）：永远字符串比较，避免 "5" == 5 类的踩坑。
//   - in：成员关系，字符串相等比对。

// whenCond 是已解析的 when 表达式；nil 表示无条件（恒真）。
type whenCond struct {
	left  operand
	op    string
	right []operand // 单元素：普通运算；多元素：in 列表
}

type operand struct {
	isVar bool
	raw   string // isVar=true 时是 path（不含 ${}），false 时是字面量原文
}

// parseWhen 解析单行 when 表达式。空字符串返回 nil（恒真）。
func parseWhen(expr string) (*whenCond, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	if hasLogicalComposition(expr) {
		return nil, fmt.Errorf("when: logical composition is not supported (use multiple reactors instead): %q", expr)
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
			return nil, fmt.Errorf("when: left %w", err)
		}
		if opNorm == "in" {
			rs, err := parseList(right)
			if err != nil {
				return nil, fmt.Errorf("when: right %w", err)
			}
			return &whenCond{left: l, op: "in", right: rs}, nil
		}
		r, err := parseOperand(right)
		if err != nil {
			return nil, fmt.Errorf("when: right %w", err)
		}
		return &whenCond{left: l, op: opNorm, right: []operand{r}}, nil
	}
	return nil, fmt.Errorf("when: no recognized operator in %q (supported: == != < <= > >= in)", expr)
}

// hasLogicalComposition rejects and/or/not-style composition outside quoted strings
// and ${...} references. v5 deliberately supports exactly one comparison per reactor.
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
