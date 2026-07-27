# Windows TUI 长多行粘贴切段事故（2026-07-27）

## 症状

Windows Terminal 的 PowerShell 标签页经 ConPTY 运行 AgentGo TUI 时，长多行剪贴板内容可能在中途被提交。Scheduler 收到的是开头残片，剩余文本仍继续注入输入框，最终形成多条互不完整的用户请求。

## 根因

终端未透传 bracketed paste 时，剪贴板会退化为高速 `KeyRunes` 与真实 `Enter` 事件流。旧实现把每个 Enter 先当换行，再等待固定 100ms 提交；ConPTY 分块注入一旦出现超过 100ms 的停顿，定时器就会在粘贴尚未结束时提交当前内容。

因此问题不在读取长度，也不能只靠扩大固定防抖窗口解决：固定窗口无法可靠区分“真人提交”与“仍在分块的粘贴”。

## 修复

`internal/tui/paste_burst.go` 引入独立输入分类状态机：

- bracketed paste 与 `Ctrl+V` 整体读剪贴板的路径保持原样；
- 无显式 paste 标记时，极短间隔的字符流进入 burst 缓冲；
- burst 内的 Enter/Tab 只作为文本控制字符，不参与全局按键分发；
- 180ms 空闲后把当前缓冲整体写入 textarea，但不自动提交；
- 缓冲刷出后的 500ms 保护窗口继续接纳 ConPTY 的下一分块；
- 单个 ASCII 候选只延迟 12ms，普通 Enter 仍立即提交；
- IME 一次提交的非 ASCII 词组立即写入，不因状态机出现可感知漏字；
- 同时最多保留一个 Bubble Tea tick，reset 后晚到的旧 tick 由代数淘汰。

安全取舍是“疑似粘贴时宁可把 Enter 当换行，也不提交不完整请求”。状态机仍是启发式：终端既不提供 bracketed paste、分块又停顿超过保护窗口时，应用无法从按键事件中获得绝对可靠的粘贴边界。此时 `Ctrl+V` 主动读取剪贴板，或把内容写入项目内文本文件后发送一条单行读取指令，仍是确定性路径。

## 回归证据

`internal/tui/paste_burst_test.go` 与 `internal/tui/paste_test.go` 覆盖：

- 普通单字符与普通 Enter；
- 高速 ASCII、多行 Enter、非 ASCII/IME；
- 分块间隙超过旧 100ms 窗口；
- buffer 已刷出但保护窗口仍生效；
- 边界键清理、单 tick 约束与旧 tick 失效；
- 最终只向控制面提交一条、内容逐字一致的多行请求。

