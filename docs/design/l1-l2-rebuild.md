# L1/L2 重建实施记录

状态：核心实施与当前可执行验收完成；macOS 原生启动验证待 CI，当前模型未声明媒体能力，因此没有进行真实图片/文件调用。代码与文档纳入本次重构提交；未推送。

## 已实施

- L1 仅接受封存 Request；请求、协议、模型、schema/strict、调用选项和预算均显式绑定。两种协议只走 SSE，HTTP 自动重试关闭。终止、取消、字段上限与输出项身份在 L1 校验。
- L2 统一角色与目标、记忆、历史、上游、工具和非文本装配；ContextSnapshot v2、Context v11、Replay v5。Snapshot、完整响应的记录端口都是模型调用的必要依赖，不存在无记录执行分支。
- 主 Agent、Scheduler、Runner、Team、Spawn、Proposal Acceptance、用户 Reactor、Observation 和启动探针均已切换。探针也使用真实操作身份与共用 L3 输出写入。
- 旧 Context/请求/Binding/数组历史拒绝；配置要求 request_contract，stream 字段拒绝；Session v6 和模型历史信封明确拒绝旧版本。旧磁盘数据没有删除或转换。
- L2 WatchModelOutput 管理 eventCursor、初始快照、增量、终态与重同步；UI Hub 只转接。Web SSE id/Last-Event-ID 与 TUI 使用同一契约，Session 切换恢复完整输出。
- 文档重写职责与文件索引；旧实施记录归档。三平台 CI、Go/Python/接口/架构约束测试已更新。

## 删除、迁移、重写与新增对账

| 类别 | 实际内容 |
|---|---|
| 删除 | 旧 Prompt Build 及 context 传递、legacy 消息/manifest builder、旧 Invoke/Client.Chat 接缝、非流式分支、SDK prompt 注入、请求语义 context override、Agent 流式回调、Hub 轮次写盘 |
| 迁移 | invocation 的有效错误与预算契约进入 llm；History DTO 进入 contextcontract；有效片段/原子组算法进入 contextruntime |
| 重写 | 请求封存、协议编码输入、typed Replay、记忆与历史装配、调用记录门、Session 版本拒绝、UI 订阅接线 |
| 新增 | 媒体内容块与独立预算、L3 授权引用读取、SSE 帧/终止校验、流式事件游标与重同步、架构回归测试和真实协议探针 |

三个旧包 `internal/prompt`、`internal/invocation`、`internal/contextadapter` 已退役。架构测试对生产 AST 验证旧符号不存在、SDK 仅由 L1 导入、模型 Invoke 仅由 L2 调用。

当前工作区删除 36 个旧 Go 文件路径，其中 18 个是生产源码路径；新增 41 个 Go 文件。路径删除包含职责迁移，不能全部算作算法删除；旧分支的删除另由上表与 AST 检查确认。

## 验证结果

完整机器记录见 [validation.json](../test-issues/2026-09-07-0136-l1-l2-rebuild-validation.json)。

| 验证 | 结果 |
|---|---|
| 全量 Go 测试、vet、构建 | 通过 |
| Python SWE Test Runner 与 SSE 解码 | 77 项通过 |
| Windows Responses / Chat 完整二进制流程 | 两者均 Graph success、final-report completed、Delivery committed |
| 新落盘契约 | 每次完整流程 30 个 ContextSnapshot、30 个模型输出；29 次 Agent 调用与账本/Trace 一致 |
| 取消记账 | 活动预留与取消后的遗留预留均为 0 |
| Linux 并发 race | llm/contextruntime/ui/session 通过 |
| Linux Chat 二进制流程 | Graph success 与真实产物断言通过 |
| 真实 Responses / Chat | 每协议工具调用、exact replay、首增量后取消，共 3 次调用通过；caller_cancelled，取消用量未知 |
| Web SSE / JS | 游标续接、文本、终态与隐藏协议载荷测试通过；JS 语法检查通过 |
| macOS | arm64/amd64 交叉构建通过；原生启动未执行，CI 待运行 |
| 图片/文件 | 本地两协议编码、完整内容、预算和授权拒绝通过；当前模型未声明该能力，真实调用未验证 |

本次没有运行正式 Flask-8 批测，没有以历史验证替代本次证据。提交前已清理过期的 Prompt Build、旧调用入口与非流式注释，并校正活动文档中的 L1/L2 职责和 Context/Replay 版本；旧 Trace 字段仅保留历史读取用途。

## 复现入口

```bash
go test ./...
go vet ./...
go build -o agentgo.exe
python -m unittest discover -s scripts/swe_test_runner -p '*_test.py'
python scripts/local_fake_provider_smoke.py --binary ./agentgo.exe
python scripts/local_fake_provider_smoke.py --binary ./agentgo.exe --protocol chat_completions
go run ./scripts/model_contract_probe -config setting.yaml -protocol responses
go run ./scripts/model_contract_probe -config setting.yaml -protocol chat_completions
```

真实探针只发送合成输入，不输出凭据、端点或供应商正文。默认媒体能力为文本，启用图片/文件需要在 model_capabilities 的 input 中声明能力、单项/总字节限制及 token 预算。
