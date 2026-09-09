# CI 进程回收与探针超时修复

原始运行：[34346670627](https://github.com/wizardG7777777/AgentGo/actions/runs/34346670627)，提交 648d33f。

- Ubuntu/macOS 的 Go 全量测试、vet、构建通过；Python 的进程清理测试失败。测试未建立独立进程组，清理函数却按进程组发信号，且吞掉最终失败。正式 Runner 已使用独立 session。
- Windows 的工具健康探针属性测试出现时序竞争；rapid 明确报告 flaky。该探针不是已退役的 Observation 工作流。
- 后续本地协议冒烟与 race 步骤被跳过，不能视为通过。

修复：测试使用正式 POSIX 启动契约；清理等待进程回收，温和终止失败后升级强制终止，最终失败明确抛出，同时允许进程自行退出的竞争。添加升级、失败和并发退出回归。

探针结果以接收时的截止状态为准：取消或已到 deadline 时不能记录成功。超时属性测试改用取消事件同步，删除两个毫秒定时器竞争的假设；通过 Go synctest 验证截止前、截止时和取消的结果规则。

本地 Python 66 项离线测试通过。三平台完整验证由修复提交的 GitHub Actions 执行；以其实际结果为准。未运行真实 SWE 或真实模型。

全量本地回归还暴露 `TestE2E_AgentStateMachineLifecycle` 的日志关闭竞争：Task 已终态不等于 Agent 收尾已完成。测试现在等待 Run 返回后关闭 Trace，避免漏掉最后一条状态事件；定向连续 30 次通过。未更改 Agent 生产状态机。
