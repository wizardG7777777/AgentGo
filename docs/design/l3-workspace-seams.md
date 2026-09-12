# L3 工作区、检视与工具 schema 接缝修复

2026-09-12。承接原文模式重构及真实 SWE 的问题调查；本次范围是 L3 工具接缝，不改 watchdog，不把既有 6/8 成绩算作本次验证。

## 1. 实现与权责

| 接缝 | 修复后的行为 | 实现 |
|---|---|---|
| Git 发现父仓库 | Git 项目在 Shell 副本中建立独立元数据，不共享宿主 index、refs、对象 hardlink 或 alternates。原项目根是仓库根时保留本地历史；linked worktree 也不复制其 .git 指针 | internal/workspace/snapshot_git.go、snapshot_git_windows.go、snapshot_git_unix.go |
| 候选与 Git 基线不一致 | 先复制冻结输入、建立基线，再覆盖本节点 dirty set；git diff 比较本节点与收到的输入候选，不能把宿主未提交修改当作本节点新修改 | workspace/shell_root.go、snapshot_git.go |
| 逻辑与物理路径混用 | read_file 文件头保留逻辑项目路径；run_shell 的相对/项目内绝对 working_dir 先按项目根校验，批准后映射到完整副本并再次检查实际目录边界 | tools/local_read.go、shell.go、workdir.go |
| Shell 撤销/删除后旧内容复活 | 差异同步遍历输入、执行后目录和已有 manifest 三者。恢复基线或删除新文件时移除旧 overlay 条目；删除已有文件保留删除事实。空文件树也能冻结和交付 | workspace/dataflow.go、manifest.go、shell_root.go |
| 尚无 Task 的节点不可检视 | 按 Graph/Node 查询可读取定义、状态和等待原因；不伪造 Task/Activation/Attempt。看板也包含没有任何已派发 Task 的当前 Run 图；摘要覆盖图状态变化 | tools/inspection.go |

Git 副本只用于执行与调查。最终文件提交仍由既有候选/Delivery 事务负责；副本里的 Git commit 不改变宿主 HEAD/index，也不被当作节点或图完成。候选不包含 .git。子目录 ProjectRoot 建立该子树的独立输入基线，不承诺完整父仓库历史。非 Git 项目不新增 Git 安装要求；声明为 Git 项目但元数据损坏时明确拒绝，不能回退父仓库。

清除会指向外部工作树/索引的继承 Git 环境变量，并设置 GIT_CEILING_DIRECTORIES。副本不保留指向主根的 origin。初始化只读取本地 Git，不调用真实模型或外部 Git 服务。每条初始化 Git 命令有两分钟执行上限；完整本地历史复制增加磁盘和准备时间，尚未针对超大仓库优化。

## 2. 过期说明删除对账

- provision_agent_team 删除“图内 controller 继承当前图”的错误说明，明确只有图外 Scheduler 创建 Team，返回 event_type 用作 agentTask.execution.route_ref。
- request_user_input 删除已不存在的 approval 节点说明。
- run_shell.working_dir、apply_change.path 更新为统一逻辑路径；inspect_node 明确未派发节点的检视方式。
- 删除无当前配置/生产调用方的 prompts/gatherer.md、verify_report.md、verify_text_submission.md、verify_program_worker.md。这些文件仍推荐 write_file、edit_file、list_dir、publish_task 等已退役工具或旧兼容任务。
- 清理通信、工作区及终态接口旁关于旧发布工具、深度限制和控制节点的残留注释。
- 对全部实际注册定义执行 schema 检查，防止退役工具/控制节点说明重新进入模型工具面；继续检查 AllToolNames 与完整注册并集相等。

本次没有删除仍被执行器使用的 workspace_input 字段，也没有重定义多候选选择契约。该字段不是不存在的工具；其产品设计调整单独处理。

## 3. 验证与剩余边界

已完成以下验证，日志位于本机 `%LOCALAPPDATA%/AgentGo/l3-seams-*.log`：

- Windows：全量 `go test ./...`、`go vet ./...`、构建与 `git diff --check` 通过；Python 68 项测试通过。
- 真实本地 Git 回归：独立 HEAD/index、源历史、未提交输入、上游候选基线、linked worktree、子目录 ProjectRoot、继承环境清理、Git 元数据损坏拒绝、快照重建、恢复/删除不复活，以及空文件树交付均通过。
- 工具回归：逻辑路径回显、相对/绝对 cwd 等价映射、非法目录拒绝、未派发节点与等待原因、跨 Run 隔离、看板状态摘要和全部注册 schema 均通过。
- Windows 新二进制本地 SSE：Responses 与 Chat Completions 普通文本结束场景各 27 次调用；Responses 动态 Team 场景 28 次。均验证文件实际修改、`git diff` 可见、下游读取同一候选、图级 Delivery、宿主 Git HEAD 不变和消息不唤醒接收者。
- Linux：Windows 交叉编译后在 WSL Ubuntu 22.04 原生执行 workspace 全包测试、上述 tools 定向测试，以及 Responses SSE 二进制场景，均通过。tools 全包在 WSL 的尝试有一项环境失败：现有 `go version` Shell 测试需要 Linux Go 命令，而该环境未安装；没有修改该测试以绕过失败。
- macOS：交叉构建通过，尚未进行原生启动。当前 Windows CGO=0 且无 gcc，Linux WSL 未安装 Go，Docker daemon 未运行，本地未运行 race；CI 的 Linux race 列表已加入 tools 包，三平台 SSE 场景保留并增加普通文本结束验证。CI 尚未提交运行，不声称已经通过。

最终 Windows 二进制 SHA256：`85917E215DEA233F2920547DD532380035C1D834CD20F8B7BCDD874CADA383F9`。验证脚本为 `scripts/local_fake_provider_smoke.py`，本地 fixture 不是外部模型能力验证。

本轮没有重跑外部真实 SWE。L5 重复 execution_blocked 事件仍是开放问题，不能因 L3 修复而标记关闭。

后续状态（2026-09-12）：L5 等待事实去重、workspace_input 删除及测试终态判读已进一步修复；本文上面的未完成项为当时状态，当前实现及真实复测见 [剩余问题修复与复测](swe-remaining-repairs.md)。
