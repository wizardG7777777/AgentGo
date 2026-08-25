package shell

import (
	"regexp"
	"strings"
)

// RedirectWritePatternPrefix 是 CommandFilter.Check 命中"重定向写文件"硬规则
// 时返回的 pattern 前缀，其后跟随命中的片段描述（如 "> out.txt"）。
// WrapShellTool 据此输出区别于普通黑名单的拒绝消息（指引改用 write_file /
// edit_file 工具）。
const RedirectWritePatternPrefix = "redirect-write:"

// PowerShell 侧写文件的等价写法。在剥离引号内容与 here-doc 正文后的命令文本
// 上匹配，避免 echo "Out-File" 之类的字面量误伤。
var (
	redirectOutFileRe   = regexp.MustCompile(`(?i)\bOut-File\b`)
	redirectTeeRe       = regexp.MustCompile(`(?i)\btee\b`)
	redirectTeeObjectRe = regexp.MustCompile(`(?i)\bTee-Object\b`)
)

// heredocSpec 描述一个待消费的 here-doc：定界符与 <<- 形式的剥 tab 语义。
type heredocSpec struct {
	delim    string
	stripTab bool
}

// detectRedirectWrite 检测命令中"写向文件的重定向"，命中时返回人类可读的
// 片段描述（如 "> out.txt"、"Out-File"、"tee"），未命中返回空串。
//
// 与黑名单一致的双方言覆盖：run_shell 在 POSIX 由 sh 解释、Windows 由
// PowerShell 解释，> / >> / fd 重定向（2>、&>、*> 等）两种方言都拦；
// Out-File / tee / Tee-Object 是 PowerShell 的等价写法，一并硬拒。
//
// 判定取舍（从严，但只拦"目标像文件路径"的重定向）：
//   - 放行：fd 复制（2>&1、>&2）、丢弃输出（>/dev/null、2>$null）、输入
//     重定向（< file）、比较语境（test 1 -gt 0 本就不含裸 > 字符）、单/双
//     引号内的字面 >（echo "a > b"）、here-doc 正文、行注释（# 到行尾）、
//     空目标（> 后无词，本来就是语法错误）。
//   - 拦截：目标为其他任意非空 token 一律视为写文件——包括变量（> $out）、
//     >("proc") 这类在 sh/PowerShell 本就是语法错误的形式，从严不多放行。
//   - PowerShell here-string（@"..."@）正文不做跳过，其中的裸 > 会被当作
//     重定向拦截——here-string 配重定向恰是要防的写法，从严。
//   - Set-Content / Add-Content / sed -i / gcc -o 等非重定向形式的写文件
//     不在本规则范围（需要时走黑名单补充）。
func detectRedirectWrite(command string) string {
	cleaned, detail := scanRedirects(command)
	if detail != "" {
		return detail
	}
	// 第二阶段：PowerShell 写文件 cmdlet / 管道。tee / Tee-Object 一律拦：
	// Tee-Object -Variable 这类不落盘的用法从严一并拦截（LLM 输出中极少，
	// 且 tee 的语义就是复制一份到文件）。
	if redirectOutFileRe.MatchString(cleaned) {
		return "Out-File"
	}
	if redirectTeeRe.MatchString(cleaned) {
		return "tee"
	}
	if redirectTeeObjectRe.MatchString(cleaned) {
		return "Tee-Object"
	}
	return ""
}

// HasPipeline 报告命令是否包含会把进程退出码折叠为“最后一个管道段”的
// 真正 pipeline 操作符。它复用 redirect scanner 对引号、注释与 here-doc
// 正文的剥离语义，并排除逻辑 OR（||）及转义后的字面竖线。
//
// run_shell 依赖它冻结 ExitCodeScope：POSIX sh 与 PowerShell 默认都只保证
// 最后一个管道段的状态，不能把 exit=0 投影成整条测试命令通过。
func HasPipeline(command string) bool {
	cleaned, _ := scanRedirects(command)
	for i := 0; i < len(cleaned); i++ {
		if cleaned[i] != '|' {
			continue
		}
		if (i > 0 && cleaned[i-1] == '|') || (i+1 < len(cleaned) && cleaned[i+1] == '|') {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && (cleaned[j] == '\\' || cleaned[j] == '`'); j-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			continue
		}
		return true
	}
	return false
}

// HasFileRedirect 报告命令是否含写文件重定向。run_check 使用它保证检查工具
// 只产生验证事实，不借检查通道改写业务文件。
func HasFileRedirect(command string) bool { return detectRedirectWrite(command) != "" }

// scanRedirects 第一阶段扫描：识别 > / >> / >| 等重定向操作符并判定目标；
// 同时返回剥离引号内容与 here-doc 正文后的命令文本（供第二阶段正则匹配）。
// detail 非空表示命中重定向写文件，此时 cleaned 是截断的部分结果（不再使用）。
func scanRedirects(command string) (cleaned string, detail string) {
	var out strings.Builder
	out.Grow(len(command))

	// here-doc 定界符队列：同一行可声明多个（cat <<A; cat <<B），
	// 换行后按声明顺序逐段消费正文。
	var pending []heredocSpec

	var quote byte // 当前引号：'\'' / '"'，0 = 不在引号内
	i, n := 0, len(command)
	for i < n {
		c := command[i]

		// ---- 引号区域：内容整体跳过（其中的 > 是字面量）----
		if quote != 0 {
			switch {
			case c == quote:
				quote = 0
				i++
			case c == '\\' && quote == '"' && i+1 < n:
				// sh 双引号内反斜杠转义（\$、\"、\\、\`）。Windows 路径
				// "C:\dir\" 结尾的 \" 在 PowerShell 里是字面量+收尾引号，
				// 这里按 sh 语义吞掉——可能漏判其后真正的重定向，属于
				// 已记录的取舍（LLM 很少在双引号路径结尾再跟重定向）。
				i += 2
			default:
				i++
			}
			continue
		}

		switch c {
		case '\'', '"':
			quote = c
			out.WriteByte(' ') // 引号内容折叠为空白，避免第二阶段跨词误配
			i++
		case '\\', '`':
			// sh 反斜杠转义 / PowerShell 反引号转义：下一字符按字面量处理，
			// 不参与重定向判定（\>、\`> 都是字面 >）。
			// 取舍：Windows 路径里的 \ 后随普通字符被跳过不影响检测；但
			// PowerShell 中 \> 实际是重定向（\ 不是 PS 转义符），统一按
			// 转义处理会漏判这一角落——LLM 输出 \> 极为罕见，接受。
			out.WriteByte(c)
			if i+1 < n {
				out.WriteByte(command[i+1])
				i += 2
			} else {
				i++
			}
		case '#':
			// 行注释（sh 与 PowerShell 一致）：# 在词首（行首或空白/操作符
			// 之后）时到行尾为止全是注释，其中的 > 不是重定向。
			if i == 0 || isCommentBoundary(command[i-1]) {
				for i < n && command[i] != '\n' {
					i++
				}
				out.WriteByte(' ')
			} else {
				out.WriteByte(c)
				i++
			}
		case '<':
			// <<< here-string：数据内联在同一行，无正文需要跳过。
			if i+2 < n && command[i+1] == '<' && command[i+2] == '<' {
				out.WriteString("<<<")
				i += 3
				continue
			}
			if spec, next, ok := parseHeredocStart(command, i); ok {
				pending = append(pending, spec)
				out.WriteString("<< ")
				i = next
			} else {
				// 单个 < 是输入重定向（读文件），放行。
				out.WriteByte(c)
				i++
			}
		case '>':
			target, next, op := parseRedirectTarget(command, i)
			out.WriteString(op)
			if target == "" {
				// fd 复制（>&2）或空目标（语法错误）：不写文件，放行。
				i = next
				continue
			}
			if isNullRedirectTarget(target) {
				// 丢弃输出：>/dev/null、2>$null，不写项目文件，放行。
				out.WriteByte(' ')
				out.WriteString(target)
				i = next
				continue
			}
			// 命中：重定向目标按文件路径处理。fd 前缀（2> / *> / &>）
			// 仅用于拒绝消息展示。
			return out.String(), fdPrefixOf(command, i) + op + " " + target
		case '\n':
			out.WriteByte(c)
			i++
			// 换行后消费所有待处理 here-doc 正文：逐行跳过直到定界符行。
			// 正文里的任何字符（包括 >）都是字面数据。
			for len(pending) > 0 {
				spec := pending[0]
				pending = pending[1:]
				i = skipHeredocBody(command, i, spec)
			}
			out.WriteByte(' ')
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), ""
}

// isCommentBoundary 报告 # 前一个字符是否使 # 处于词首（构成行注释）。
func isCommentBoundary(prev byte) bool {
	switch prev {
	case ' ', '\t', '\n', '\r', ';', '|', '&', '(', ')', '<', '>':
		return true
	}
	return false
}

// parseHeredocStart 解析 command[i]('<') 处是否是 here-doc 起始
// （<<EOF / <<-EOF / <<'EOF' / <<"EOF"）。是则返回定界符与消耗完
// <<、修饰符、定界符之后的位置；否则 ok=false（含 <<< here-string，
// 其数据内联无正文）。
func parseHeredocStart(command string, i int) (heredocSpec, int, bool) {
	n := len(command)
	if i+1 >= n || command[i+1] != '<' {
		return heredocSpec{}, i, false
	}
	if i+2 < n && command[i+2] == '<' {
		return heredocSpec{}, i, false
	}
	j := i + 2
	spec := heredocSpec{}
	if j < n && command[j] == '-' {
		spec.stripTab = true
		j++
	}
	for j < n && (command[j] == ' ' || command[j] == '\t') {
		j++
	}
	// 定界符可带引号（'EOF' / "EOF"），引号只表示正文不做参数展开。
	var delim strings.Builder
	var q byte
	for j < n {
		ch := command[j]
		if q != 0 {
			if ch == q {
				q = 0
			} else {
				delim.WriteByte(ch)
			}
			j++
			continue
		}
		switch ch {
		case '\'', '"':
			q = ch
			j++
		case ' ', '\t', '\n', '\r', ';', '|', '&', '<', '>', '(', ')':
			if delim.Len() == 0 {
				return heredocSpec{}, i, false
			}
			spec.delim = delim.String()
			return spec, j, true
		default:
			delim.WriteByte(ch)
			j++
		}
	}
	if delim.Len() == 0 {
		return heredocSpec{}, i, false
	}
	spec.delim = delim.String()
	return spec, j, true
}

// skipHeredocBody 从 command[i]（某行行首）开始逐行跳过 here-doc 正文，
// 直到内容恰为定界符的结束行（<<- 形式允许前导 tab），返回结束行之后的位置。
// 找不到结束行时返回 len(command)：未闭合的 here-doc 把剩余内容全当正文——
// 此时命令本身执行不了，漏判优于误判。
func skipHeredocBody(command string, i int, spec heredocSpec) int {
	n := len(command)
	for i < n {
		lineStart := i
		for i < n && command[i] != '\n' {
			i++
		}
		line := strings.TrimSuffix(command[lineStart:i], "\r")
		if i < n {
			i++ // 吞掉换行
		}
		if spec.stripTab {
			line = strings.TrimLeft(line, "\t")
		}
		if line == spec.delim {
			return i
		}
	}
	return i
}

// parseRedirectTarget 解析 command[i]('>') 处的重定向操作符与目标。
// 返回目标 token、目标结束后的扫描位置、操作符原文（> / >> / >|）。
// fd 复制 / 关闭（>&2、2>&1、>&-）不写文件，返回空目标；空目标（行尾等）
// 也返回空串，由调用方放行（命令本身是语法错误）。
func parseRedirectTarget(command string, i int) (target string, next int, op string) {
	n := len(command)
	j := i + 1
	op = ">"
	if j < n && command[j] == '>' {
		op = ">>"
		j++
	}
	// >| 是 POSIX noclobber 强制覆盖写，同样写文件。
	if j < n && command[j] == '|' {
		op += "|"
		j++
	}
	for j < n && (command[j] == ' ' || command[j] == '\t') {
		j++
	}
	// fd 复制 / 关闭：>&2、2>&1、>&- —— 目标是另一个 fd，不写文件。
	if j < n && command[j] == '&' {
		j++
		for j < n && ((command[j] >= '0' && command[j] <= '9') || command[j] == '-') {
			j++
		}
		return "", j, op
	}
	target, next = readWord(command, j)
	if target == "" {
		return "", j, op
	}
	return target, next, op
}

// readWord 读取一个 shell 词（重定向目标），处理引号与转义，
// 返回词内容（引号已剥除）与结束位置。
func readWord(command string, j int) (word string, next int) {
	var b strings.Builder
	i, n := j, len(command)
	for i < n {
		c := command[i]
		switch {
		case c == '\'' || c == '"':
			q := c
			i++
			for i < n && command[i] != q {
				b.WriteByte(command[i])
				i++
			}
			if i < n {
				i++ // 跳过收尾引号
			}
		case c == '\\' || c == '`':
			if i+1 < n {
				b.WriteByte(command[i+1])
				i += 2
			} else {
				i++
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
			c == ';' || c == '|' || c == '&' || c == '<' || c == '>' ||
			c == '(' || c == ')':
			return b.String(), i
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i
}

// isNullRedirectTarget 报告重定向目标是否是"丢弃输出"的空设备——这类
// 重定向不写项目文件，放行。$null 大小写不敏感（PowerShell 惯例）。
func isNullRedirectTarget(target string) bool {
	return target == "/dev/null" || strings.EqualFold(target, "$null")
}

// fdPrefixOf 取 > 前紧邻的 fd 前缀（数字 / * / &），仅用于拒绝消息展示。
// 数字前缀要求与前面的字符有边界（空白/操作符/行首），避免把普通参数
// 误读成 fd；展示差异不影响拦截判定本身。
func fdPrefixOf(command string, i int) string {
	k := i - 1
	for k >= 0 && ((command[k] >= '0' && command[k] <= '9') || command[k] == '*') {
		k--
	}
	if k == i-1 {
		// 紧邻字符不是数字/*：检查 &>（bash 合并重定向；dash 解析为
		// 后台+截断，PowerShell 是语法错误——无论哪种都按写文件拦）。
		if i > 0 && command[i-1] == '&' {
			return "&"
		}
		return ""
	}
	if k < 0 || isCommentBoundary(command[k]) {
		return command[k+1 : i]
	}
	return ""
}
