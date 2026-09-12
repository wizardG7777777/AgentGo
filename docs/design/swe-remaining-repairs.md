# SWE 剩余问题修复与复测

2026-09-12。目标是修复上一轮真实 Flask-8 遗留问题，并以独立测试根运行完整八题，逐题分析实际效果。修复、离线验证和正式完整批次均已完成，本版本 8/8 成功；前次 6/8 保留为对照。

## 删除、迁移、重写、新增对账

| 类别 | 实现 | 行为 |
|---|---|---|
| 删除 | graph/dataflow_contract.go、tools/graph_schema.go、scheduler 提示词 | 删除模型 workspace_input 字段及其校验/选择路径；不增加字段别名 |
| 迁移 | workspace/candidate_baseline.go、bootstrap/dataflow_runtime.go | 基线选择由运行时读取不可变候选 ParentRef 谱系：文本不充当代码，唯一候选直接继承，同谱系取后继，独立分支明确报 candidate_branch_conflict |
| 重写 | graph/dataflow_runtime.go、dataflow_store.go | WaitingFingerprint 覆盖节点定义、原因与直接输入事实；相同等待不写日志、不制造新规划事件；确认及重启保留去重语义，实际变化照常发布；重复派发失败原因同样去重 |
| 重写 | scripts/swe_test_runner/runner.py、runtime_audit.py | 删除固定 30 秒终态稳定窗口，当前 UI 终态必须由持久化调用/工具/reservation、TaskOutcome、Completion/Delivery 事实确认；deadline 时再取新快照区分已结算与未完成 |
| 修正 | internal/agenttemplate/prompts、scheduler | 内置 Team 模板也允许最终纯文本结束，移除与运行时不符的强制提交说明 |
| 新增验证 | dataflow_waiting_test、candidate_baseline_test、runner_test、本地 SSE 脚本 | 多等待节点、规划确认、重启、原因变化、自动派发、候选谱系/分支、旧字段拒绝、deadline 新快照和结算边界 |

## 契约与边界

Graph 配置、定义、存储 schema 统一升至 `agentgo.graph/v7`，新目录 `.agentgo/state/graphs-v8`。v6 与旧 workspace_input 输入明确拒绝；原目录保留，不迁移或重新执行旧图。L1/L2 原文模式、880000 tokens SWE 配置及其它现行数据契约保持。

CandidateRef/ResultRef 是运行时业务身份，保留。此改动不提供独立候选分支的自动合并，也不让普通消息修改图。缺少输入仍显示等待，不能伪造结果。watchdog 生产代码未修改。

Python 结果追加 deadline_reached、completion_observed、settlement_verified；完整日志保留原执行事实，终端只展示判读所需字段。最终 Judge 仍独立验证主根源码、测试身份、实际 Flask 导入位置、失败集合及补丁，不由 AgentGo 声称测试通过。

## 验证与真实复测

Windows 全量 Go 测试/vet/构建、70 项 Python 测试、双协议及 Team 二进制用例通过；Linux Graph/Workspace/Tools race 通过，macOS 双架构交叉构建通过。正式批次记录见 [完整报告](../test-issues/2026-09-12-flask8-after-context-and-l3-fixes.md)。本次真实测试继续使用 Flask-8 完整 tasks.csv 和每题 1200 秒外部窗口，遇 provider HTTP 错误或必需变量问题立即停止。测试期间不修改受测代码、提示词或向 Agent 注入修复建议。

首个诊断批次 `swe-repairs-v7-batch-20260912-023301` 在第一题暴露 Git for Windows 的 packed object 长路径失败，已停止并保留 diagnostic-stop.json 和原日志，不作为完整批次成绩。修复为 Git 准备命令和副本本地配置均显式启用 core.longpaths；新增超过 260 字节 pack/index 路径的实际 Git 回归。该测试区分 Git 文件路径与 Windows 进程 cwd 的系统限制，不通过修改用户全局 Git 配置隐藏问题。

正式测试根：`C:/Users/73524/AppData/Local/AgentGo/swe-repairs-v7-batch-20260912-024222`。二进制 SHA256 `50781747C968824FD70D69B24F1F5990867317652C15C9AF3D32FA28EC493481`；8/8 修复成功，335 次调用，AgentGo 合计 1554 秒，无新增失败或未结算项。残余参数/路径/引用可用性问题见报告，不宣称全部工具调用无错。
