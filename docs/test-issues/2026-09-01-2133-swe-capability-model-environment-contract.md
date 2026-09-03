# SWE Test Runner 能力档位模型环境契约

> 日期：2026-09-01
> 状态：Implemented / local and live mixed-model contract verified
> 问题编号：SWE-118

## 1. 背景

旧入口只读取 `SWE_MODEL`，并把同一模型渲染给 Scheduler、Explorer、Worker、
Verifier 与全局默认模型。该契约可以衡量单一模型能力，却不能验证“低成本控制面 +
强执行模型”的同 provider 协作构想。

当前 Flask-8 失败均发生在 Worker 执行链，但环境变量不应提前固化角色职责；
同 kind replicas 依配置保持同质，具体角色分工应由专用 YAML 显式审阅。

## 2. 新契约

公开入口现在要求四个非空环境变量：

- `SWE_API_KEY`；
- `SWE_BASE_URL`；
- `SWE_FAST_MODEL`：快速模型能力档位；
- `SWE_FLAG_SHIP_MODEL`：旗舰模型能力档位。

两个模型变量允许相同以复现单模型基线，也允许不同以测试同一 endpoint/key 下的
分层协作。它们不拥有角色语义；角色到能力档位的绑定以
`setting.swe-flask.yaml` 为权威。旧 `SWE_MODEL`、`SWE_BASE_MODEL`、
`SWE_WORKER_MODEL` 不读取且不作为 fallback；缺项仍在任何网络、目录、worktree
或子进程副作用前一次性聚合报告。

## 3. 执行与审计

- `setting.swe-flask.yaml` 分别渲染 fast/flag_ship 占位符；当前默认由快速模型承担
  Scheduler、Explorer、Verifier 与全局默认，旗舰模型承担两个同质 Worker replicas。
  后续重分工只修改该 YAML，不改变环境契约。
- 前置 typed function-call probe 按模型名去重；相同模型只请求一次，不同模型逐一
  验证，任一失败都在启动 AgentGo 前 fail-closed。
- 每题 `result.json` 写入 `model_capabilities`，冻结 fast/flag_ship 两个注入值；
  角色映射由同目录渲染后的 `setting.yaml` 审计。
- 两个模型仍共享 `SWE_BASE_URL` 与 `SWE_API_KEY`；本变更不引入多 provider、fallback
  或按具体模型名分支。

## 4. 验证

- SWE Test Runner Python 单测 70 项通过；覆盖旧模型变量不能兜底、四项缺失聚合、
  两能力档位去空白、YAML 角色分配、不同模型双 probe 与相同模型 probe 去重。
- 2026-09-02 已完成混合 `qwen3.8-flash` / `qwen3.8-max` 真实 provider probe、
  Observation v7/v8 × empty/populated 矩阵与三道定向题；矩阵八组均 3/3，三题
  `model_contract_compatible=true`。完整 batch 仍受 SWE-119 记录的非本阶段
  Recovery v4 架构门阻断，不能宣称业务通过率已提升。
