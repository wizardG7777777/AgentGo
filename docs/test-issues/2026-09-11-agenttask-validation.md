# agentTask 数据流图验证记录（2026-09-11）

## 已验证范围

- Windows Go 全量测试、go vet、构建通过。
- 新图/候选/交付测试覆盖多输入就绪、增量 CAS、冻结副本、失败事实输入、取消回执、完成幂等与 unknown 不重放。
- 本地真实二进制 SSE fixture：Responses、Chat Completions、动态 Team；单调查图启动后追加 work/check，实际交付文件；连续读取8次不触发旧观察关卡；普通消息不创建任务。
- Python 离线67项通过，包含新 runtime audit、Graph 摘要核验、最终 Judge 的输入身份和失败集合比较。
- Linux/WSL race：graph、workspace、agent、contextruntime 均通过。三平台完整 CI 记录以本提交对应 GitHub Actions 的“验证”工作流为准，不以本地模拟代替 macOS 运行。

## 首次真实 SWE

独立目录：`C:/Users/73524/AppData/Local/AgentGo/swe-agenttask-v6-20260910-201206`。原来的 swe/runs 未覆盖。

| 项目 | 事实 |
|---|---|
| 题目 | automatic-options |
| Run | run-swe-automatic-options-71eb0ba8-e462-4a69-95b8-8ac60d67798a |
| 基线 | 2 failed、492 passed |
| 最终 Judge | 494 passed、0 failed；resolved |
| 补丁 | 28行；tests 未被篡改 |
| 图与交付 | graph completed/success，实际 Delivery 已提交 |
| 运行审计 | architecture_ok=true、task_resolved=true；无悬挂调用/工具/预约和 Outcome 投递缺口 |
| 监控耗时 | 104秒（不含全部准备/判题） |
| 模型使用 | 22次调用；prompt 262216、completion 9451 tokens |
| 建图 | 1次 apply_graph_change，未反复创建；control_graph 启动和完成各一次 |
| 二进制SHA256 | 9BCE9E3CA04B56B198B66DE7731E76864E5DE3F57FFBB024375C3C658AF68D37 |

原始证据为该目录 runs/automatic-options 下的 result.json、judge.json、model.patch、各阶段 pytest execution/report、snapshot.final.json，以及 worktree 下 Graph/Outcome/Delivery/Trace。没有将旧 Trace 转换为新请求执行。

此后删除了不再被调用的旧控制类型，租约切到 v4，并补充取消/失败输入/完成预检和 UI 字段清理。最终二进制的同题复核结果另列在下节，不能把第一次运行冒充后续构建的字节身份。

## 最终构建复核

最终目录：`C:/Users/73524/AppData/Local/AgentGo/swe-agenttask-v6-final-20260910-210044`。

- Run：`run-swe-automatic-options-0dc617a2-3ceb-4c12-a51e-ae6687b73bc4`。
- 最终二进制 SHA256：`B7DB9B00FCA05FB36B3667B0A154827044BC582A74CDFE90522CD7011FDD48BF`。
- 完整四阶段执行，494 passed、0 failed，23行补丁，tampered=false。
- architecture_ok=true、task_resolved=true、execution_complete=true；graph completed/success，无 hard kill。
- 监控104秒，18次模型调用，prompt 213493、completion 10864 tokens。
- 未出现用户指定的 provider HTTP 错误停止条件。

两次真实运行独立保留。第二次为最终清理后的构建复核；补丁行数不同属于两次模型生成的差异，不能用首次补丁冒充第二次结果。

## 边界

没有运行整个 Flask-8；不报告其它七题通过。真实服务只验证了配置的 Responses 文本/工具调用，不据此宣称所有 provider、图片/文件或 Chat Completions 真实端点通过。本地两协议 fixture 是确定性协议与产品接线测试。

文件提交保留 Effect 与回执；不支持多候选自动合并，不将多文件写入宣传为 OS 原子操作。历史 Session 不自动续跑。TTY 专属交互仍需对应设备人工核对。
