# KNOWN_ISSUES — 当前限制与验证缺口

最后核对：2026-09-09。工具重建的删除对账见 [清单](../design/tool-taxonomy-deletion-ledger.md)，阶段验证见 [工具契约第 13 章](../design/tool-taxonomy-and-contracts.md)。旧问题清单和原验证事实已 [归档](../archived/known-issues-before-four-categories.md)，不作为新版本通过的证据。

## 尚未完成的外部验证

- **新版真实 SWE 暂缓**：按用户要求未运行真实 probe/task/batch/verify-candidates。新工具已通过本地双协议二进制与 Python 离线回归，但不能据此报告 Flask-8 成功率或真实模型行动能力。
- **macOS 原生启动/交互未在本地验证**：CI 保留 macOS/Windows/Linux，当前主机为 Windows。不能以交叉构建代替 macOS 实际启动或 TTY 验证。
- **真实 provider 图片/文件能力未验证**：类型化输入的本地编码、授权与预算检查不代表当前外部模型已支持这些能力；默认仍只声明文本。
- **TTY 专属交互仍有人工验证缺口**：TUI inline/alt-screen 切换、Windows ConPTY 粘贴和终端滚动有单测，实际终端差异仍需对应设备复测。

## 当前功能边界

- **Graph 联合交付未开放**：Graph v5 仍采用单 mutable producer 的 Delivery 基线，没有多候选的原子联合 promotion；不能通过手工构造 JSON 绕过编译期约束。
- **OR 汇合需要额外关联语义**：当前静态端口单赋值规则不支持多个互斥来源共享普通节点/同一 barrier 端口。并行 AND 使用不同端口；复杂 OR 必须先设计 flow generation/correlation 契约。
- **Effect unknown 不自动重跑**：发生崩溃/取消且副作用无法确认时，保留 unknown 及候选证据，需按明确恢复/人工处理流程收口，不能为追求完成率自动重放。
- **Session 不自动续跑**：进入可接受版本的历史只恢复上下文，非终态 Task 阻断、图停驻；新的用户请求才驱动新运行。旧 Session 不转换到当前版本。
- **同一 ProjectRoot 不并发启动多个运行时**：进程内锁不替代跨进程状态存储互斥。外部 SWE Test Runner 的逐题锁只保护其自身工作目录，不能作为产品多实例支持的证明。
- **Web 是管理面**：支持输入、模式、会话和交互操作；远端访问必须按现有 token/监听配置限制，不能当作公开只读看板。
- **凭证与能力可能在探针后变化**：startup_probe=off/tcp 不验证模型请求能力；tool 探针成功也不保证账户余额、权限或后续模型行为始终可用。
- **业务结论需要证据评估**：引用可解引用、命令成功和数据完整不等于结论正确。测试判题归 Python，代码修复质量与模型对证据的解释仍需实际评测。

## 维护规则

2026-09-09 CI 暴露的进程组测试契约与探针超时竞争已修复；原始失败及修复范围见 [CI 修复记录](../test-issues/2026-09-09-ci-process-and-probe.md)。跨平台验证以修复提交的 Actions 结果为准。

只记录当前可复现的未解决问题、明确功能边界或真实验证缺口。修复后移除条目并保留原始证据；不要把历史已关闭的问题、设计愿望或未经复现的推测当作当前 bug。SWE-121/122/123 的旧日志继续保存在 2026-09-07 审计文档中，新工具不再使用其退役判据。
