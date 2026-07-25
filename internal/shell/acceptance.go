package shell

// acceptance.go 实现"验收任务 run_shell 策略收紧"（2026-07-25）。
//
// 背景：正式验收 runner（verifier kind / acceptance 节点）不授予文件写工具，
// 但 run_shell 不是只读沙箱（docs/activate/AgentTemplate.md:148 明示），
// verifier 理论上能通过 shell 污染被验收对象（包管理写、git 写、
// Set-Content、rm 等）。在 worktree 隔离（架构级方案，另案）落地前，
// 验收语境下的写倾向 shell 命令一律升级为灰名单 Interaction 审批。
//
// 接线方式：dependency_map.go 在验收角色（工具白名单含
// submit_acceptance_result）的 ShellGroup 上注入 ExtraGreylist =
// AcceptanceHardeningGreylist；tools.ShellGroup.Register 经
// DeriveWithExtraGreylist 派生过滤器。非验收语境不注入，行为完全不变。

// AcceptanceHardeningGreylist 是验收语境的加固灰名单模式集（双方言：
// run_shell 在 POSIX 由 sh 解释、Windows 由 PowerShell 解释）。
//
// 设计取舍：
//   - 只收"写倾向"命令，且一律走灰名单 ask（用户可 allow_once / allow_session），
//     不动黑名单——误伤代价是一次人工审批，而不是验收流程被硬拒。
//   - 验收正当命令必须保持放行：go test / go build / go vet、grep /
//     Select-String、读操作、git status / git diff / git log / git show 等
//     均不命中下列模式（见 acceptance_test.go 的放行矩阵）。
//   - 与 DefaultGreylist 有少量重叠（git push / npm install -g 等）：派生
//     过滤器按"先默认灰名单、后本列表"的顺序匹配，重叠命令仍报告默认模式，
//     行为与单过滤器时期一致。
//   - 已知缺口（有意不收，留给 worktree 隔离兜底）：mkdir / New-Item /
//     touch 等"创建"类命令、git tag / git config / git stash list 等
//     读写在同个子命令上的形态（bare 形态是正当读操作，从严会误伤）。
var AcceptanceHardeningGreylist = []string{
	// ---- 包管理与依赖写：改写 go.mod/go.sum、node_modules、Cargo.toml、
	// site-packages 或系统包，直接污染被验收对象或其构建环境 ----
	`(?i)\bnpm\s+(install|ci|add)\b`,                      // npm install / npm ci / npm add
	`(?i)\byarn\s+(add|remove|install)\b`,                 // yarn 依赖写
	`(?i)\bpnpm\s+(add|remove|install)\b`,                 // pnpm 依赖写
	`(?i)\bpip3?\s+install\b`,                             // pip / pip3 install（含 python -m pip install）
	`\bgo\s+get\b`,                                        // go get（改写 go.mod / go.sum）
	`\bgo\s+mod\s+(tidy|vendor)\b`,                        // go mod tidy / vendor（改写 go.mod / go.sum / vendor 树）
	`(?i)\bapt(-get)?\s+(install|remove|purge|upgrade)\b`, // apt / apt-get 系统包写
	`(?i)\bbrew\s+(install|uninstall|upgrade)\b`,          // brew 系统包写
	`(?i)\bcargo\s+(add|remove|install)\b`,                // cargo 依赖写（Cargo.toml / Cargo.lock）
	`(?i)\bdotnet\s+(add|remove)\b`,                       // dotnet add/remove（改写 .csproj）

	// ---- 版本控制写：改索引 / 提交历史 / 工作区 / 远程 ----
	// 只列写子命令；status / diff / log / show / branch 等只读子命令不在列。
	// git checkout 收窄前默认灰名单只拦 "git checkout ."，此处覆盖全部 checkout。
	`\bgit\s+(add|commit|checkout|reset|clean|rm|push|pull|merge|rebase|revert|cherry-pick|stash|apply|am|switch|restore|mv|init)\b`,

	// ---- 删除 / 移动 / 复制（双方言：Unix 命令 + PowerShell cmdlet 与别名）----
	`\brm\b`,                   // Unix rm，兼 PowerShell rm 别名
	`\brmdir\b`,                // Unix rmdir，兼 PowerShell rmdir 别名
	`(?i)\bRemove-Item\b`,      // PowerShell 删除（ri/rd 别名不收：与 grep -ri、./rd/ 包路径冲突）
	`(?i)\b(del|erase)\b`,      // cmd/PowerShell 删除别名
	`\bmv\b`,                   // Unix mv，兼 PowerShell mv 别名
	`(?i)\bMove-Item\b`,        // PowerShell 移动
	`(?i)\bRename-Item\b`,      // PowerShell 重命名（mv 同族）
	`\bcp\b`,                   // Unix cp，兼 PowerShell cp 别名
	`(?i)\bCopy-Item\b`,        // PowerShell 复制
	`(?i)\b(robocopy|xcopy)\b`, // Windows 复制工具

	// ---- 直接文件内容写（非重定向形式；redirect.go 明确不覆盖这些写法）----
	`(?i)\bSet-Content\b`, // PowerShell 覆盖写文件
	`(?i)\bAdd-Content\b`, // PowerShell 追加写文件
	`\bsed\s+(-[a-zA-Z0-9.]+\s+)*-[a-zA-Z0-9.]*i[a-zA-Z0-9.]*(\s|$)`, // sed -i 原地编辑（含 -i.bak / -Ei 等捆绑形式）
	`\bsed\s+(-[a-zA-Z0-9.]+\s+)*--in-place\b`,                       // sed --in-place（GNU 长选项）
}

// DeriveWithExtraGreylist 返回一个派生过滤器：黑名单与灰名单沿用 f，
// 灰名单末尾追加 extra（匹配顺序：先 f 原有灰名单、后 extra——重叠命令
// 仍报告 f 的原有模式，allow_session 捕获的也是原有模式）。
//
// 运行时白名单与 f 共享同一份状态：allow_session 授权的"本次运行始终允许"
// 对派生过滤器与 f 同样生效，保持全局共享单过滤器时期的语义。extra 中的
// 模式只存在于派生过滤器的灰名单，不会改变 f 自身的灰名单判定——非验收
// 语境使用 f 的 agent 行为完全不变。
func (f *CommandFilter) DeriveWithExtraGreylist(extra []string) *CommandFilter {
	grey := make([]string, 0, len(f.greyRaw)+len(extra))
	grey = append(grey, f.greyRaw...)
	grey = append(grey, extra...)
	derived := NewCommandFilter(f.blackRaw, grey)
	derived.wl = f.wl // 共享运行时白名单状态
	return derived
}
