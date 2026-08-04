package trace

import (
	"strings"
	"testing"
)

// V6 §7.4 默认脱敏：schema-aware redaction 规则断言。

func TestRedactArgs_WriteFile(t *testing.T) {
	args := map[string]any{
		"path":    "docs/a.md",
		"content": "机密正文内容",
	}
	out := RedactArgs("write_file", args)

	if out["path"] != "docs/a.md" {
		t.Errorf("path 应原样保留，实际 %v", out["path"])
	}
	redacted, ok := out["content"].(string)
	if !ok || !strings.HasPrefix(redacted, "<redacted len=") || !strings.Contains(redacted, "sha256=") {
		t.Errorf("content 应替换为 <redacted len=N sha256=前12>，实际 %v", out["content"])
	}
	// 原 map 不得被修改（还要继续参与工具 dispatch 与 ToolCallRecord）
	if args["content"] != "机密正文内容" {
		t.Errorf("入参 map 被修改：%v", args["content"])
	}
}

func TestRedactArgs_EditFile(t *testing.T) {
	out := RedactArgs("edit_file", map[string]any{
		"path":     "a.go",
		"old_str":  "old code",
		"new_str":  "new code",
		"expected_hash": "abc123",
	})
	if out["path"] != "a.go" {
		t.Errorf("path 应保留，实际 %v", out["path"])
	}
	for _, k := range []string{"old_str", "new_str"} {
		s, _ := out[k].(string)
		if !strings.HasPrefix(s, "<redacted len=") {
			t.Errorf("%s 应脱敏，实际 %v", k, out[k])
		}
	}
	if out["expected_hash"] != "abc123" {
		t.Errorf("expected_hash 属结构字段应保留，实际 %v", out["expected_hash"])
	}
}

func TestRedactArgs_PublishTask(t *testing.T) {
	out := RedactArgs("publish_task", map[string]any{
		"description": "实现一个巨大的功能，细节略三千字",
		"event_type":  "code",
		"priority":    "high",
		"expected_artifacts": "a.go,b.go",
	})
	if s, _ := out["description"].(string); !strings.HasPrefix(s, "<redacted len=") {
		t.Errorf("description 应脱敏，实际 %v", out["description"])
	}
	if out["event_type"] != "code" || out["priority"] != "high" {
		t.Errorf("event_type/priority 属结构字段应保留：%v", out)
	}
	if out["expected_artifacts"] != "a.go,b.go" {
		t.Errorf("expected_artifacts 短清单应原样保留，实际 %v", out["expected_artifacts"])
	}
}

func TestRedactArgs_SendMessage(t *testing.T) {
	out := RedactArgs("send_message", map[string]any{
		"to":       "worker-1",
		"content":  "正文：请把 x 改成 y",
		"msg_type": "info",
	})
	if out["to"] != "worker-1" || out["msg_type"] != "info" {
		t.Errorf("to/msg_type 属结构字段应保留：%v", out)
	}
	if s, _ := out["content"].(string); !strings.HasPrefix(s, "<redacted len=") {
		t.Errorf("正文应脱敏，实际 %v", out["content"])
	}
}

func TestRedactArgs_ShellCommandTruncated(t *testing.T) {
	longCmd := strings.Repeat("x", 500)
	out := RedactArgs("run_shell", map[string]any{"command": longCmd, "timeout_sec": 30})
	s, _ := out["command"].(string)
	// 截 200 字符 + 截断标记（不是 <redacted> 占位——命令是排障高价值字段）
	if !strings.HasPrefix(s, strings.Repeat("x", 200)) || !strings.Contains(s, "截断 len=500") {
		t.Errorf("command 应截断保留前 200 字符并附标记，实际 %.50q...", s)
	}
	if out["timeout_sec"] != 30 {
		t.Errorf("数字标量应原样保留，实际 %v", out["timeout_sec"])
	}
}

func TestRedactArgs_LongPathPreserved(t *testing.T) {
	// path 属结构字段：即使超过 200 字符也不脱敏（read-set-write Reactor 消费面）
	longPath := strings.Repeat("目录/", 80) + "file.go"
	out := RedactArgs("read_file", map[string]any{"path": longPath})
	if out["path"] != longPath {
		t.Errorf("长 path 应原样保留（Reactor 消费面），实际被改为 %.30q...", out["path"])
	}
}

func TestRedactArgs_LongTextDefaultRule(t *testing.T) {
	// 未列名字段：短串保留，长串（>200 字符）按 <redacted> 占位替换
	long := strings.Repeat("长", 201)
	out := RedactArgs("submit_task_result", map[string]any{
		"summary": "短摘要",
		"detail":  long,
	})
	if out["summary"] != "短摘要" {
		t.Errorf("短字符串应保留，实际 %v", out["summary"])
	}
	s, _ := out["detail"].(string)
	if !strings.HasPrefix(s, "<redacted len=201 sha256=") {
		t.Errorf("长字符串应脱敏为 <redacted len=201 ...>，实际 %.40q", s)
	}
}

func TestRedactArgs_DigestStable(t *testing.T) {
	content := "同一份正文"
	a := RedactArgs("write_file", map[string]any{"content": content})
	b := RedactArgs("write_file", map[string]any{"content": content})
	if a["content"] != b["content"] {
		t.Errorf("digest 不稳定：%v vs %v", a["content"], b["content"])
	}
	c := RedactArgs("write_file", map[string]any{"content": "另一份正文"})
	if a["content"] == c["content"] {
		t.Errorf("不同原文不应同占位：%v", a["content"])
	}
}

func TestRedactArgs_NestedValue(t *testing.T) {
	// 非标量值：JSON ≤200 原样保留，超长按长文本脱敏
	small := map[string]any{"id": "n1"}
	out := RedactArgs("submit_graph", map[string]any{"node": small})
	if m, ok := out["node"].(map[string]any); !ok || m["id"] != "n1" {
		t.Errorf("短非标量应原样保留，实际 %v", out["node"])
	}
	big := map[string]any{"text": strings.Repeat("y", 300)}
	out = RedactArgs("submit_graph", map[string]any{"node": big})
	if s, _ := out["node"].(string); !strings.HasPrefix(s, "<redacted len=") {
		t.Errorf("超长非标量应脱敏，实际 %v", out["node"])
	}
}

func TestRedactArgs_FullArgsBypass(t *testing.T) {
	SetFullArgsEnabled(true)
	t.Cleanup(func() { SetFullArgsEnabled(false) })

	args := map[string]any{"content": "完整保留", "path": "a.go"}
	out := RedactArgs("write_file", args)
	if out["content"] != "完整保留" {
		t.Errorf("FULL_ARGS 开关下 content 应完整保留，实际 %v", out["content"])
	}
	longCmd := strings.Repeat("z", 500)
	if got := RedactShellCommand(longCmd); got != longCmd {
		t.Errorf("FULL_ARGS 开关下命令应完整保留，实际截断为 %.30q...", got)
	}
}

func TestRedactShellCommand(t *testing.T) {
	short := "go test ./..."
	if got := RedactShellCommand(short); got != short {
		t.Errorf("短命令应原样保留，实际 %q", got)
	}
	long := strings.Repeat("c", 300)
	got := RedactShellCommand(long)
	if !strings.HasPrefix(got, strings.Repeat("c", 200)) || !strings.Contains(got, "截断 len=300") {
		t.Errorf("长命令应截 200 字符并附标记，实际 %.40q", got)
	}
}
