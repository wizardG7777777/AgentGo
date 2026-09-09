> **版本边界（2026-09-09）**：本页保留旧版本机制与验证事实。新运行采用 [四类工具契约](tool-taxonomy-and-contracts.md) 和 [当前冻结基线](contract-freeze-2026-08-30.md)；旧工具、强制 Observation、CheckContract 与默认阶段/进展交接条目已退役，不能据本页重新引入。仍沿用的 Delivery/Effect 不变量由当前实现与测试约束。

# Graph v4 Candidate Repair

> 状态：Implemented / targeted live verification complete  
> 日期：2026-09-03

`agentgo.graph/v4` 是 v3 Delivery Transaction 的窄扩展。新 authoring 使用
framework-owned mutating `simple-task/v4`；历史 `simple-task/v2/v3` definition 不迁移。
它不开放一般多 producer 或 OR mux。

## 拓扑

```text
investigate(route=explore, read-only)
  -> work(code-change, Delivery workspace)
     -> acceptance -> typed ends
     -> recovery(v5) -> repair(same Delivery)
                        -> acceptance-repair -> typed ends
```

- 图必须且只能有一个主 mutating producer。
- 额外 mutating producer 必须声明 `recovery_target=candidate-repair/v1`，并且只有
  一条来自 RecoveryDelta v5 controller 的 replay 入边。
- repair 从失败 work 重放原 investigation input，并从 recovery transition 继承同一
  DeliveryID；它固定使用 `progress:code-change/v10`，避免在没有下一条 recovery 边时
  再次产生 candidate handoff。Acceptance 继续在候选 workspace 验收，pass 后才
  promotion。
- v3 仍拒绝多 mutating producer；v1-v3 snapshot 不迁移。

## Investigation boundary v2

simple-task/v4 的 Explorer 使用 `progress:investigation/v6` 六轮有界读取、8分钟
downstream reserve，并冻结
`agentgo.investigation-boundary/v2` OutputContract。completed 必须先提交第一条具体
failure_observation（含 failure_kind），再提交
`public_entry`、`state_owner`、`internal_consumer` 三段对象；每段的
path/symbol/start_line/end_line 必须逐字命中 evidence_ranges，且三段至少覆盖两个
不同 path/symbol。`rejected_alternative` 必须非空。L3 在 TaskOutcome 提交前验证这些
跨字段关系，并通过 pathutil 绑定 ProjectRoot 重读声明行段；symbol 不存在、range
越界或路径逃逸/敏感均拒绝。boundary/v1 与 simple-task/v2/v3 保持历史语义。
同一冻结契约先由 `submit_task_result` 在 finalizing 前预检：范围或字段错误作为
可修正 tool error 返回，不写入 SubmitState；修正重交通过后，TaskOutcome authority
在 durable commit 再次校验，防止绕过工具入口。

## RecoveryDelta v5

Runtime 从 failure context 绑定：source activation、DeliveryID、成功 dirty paths 与
latest typed check。Controller 不能填写 `candidate_state`。

L3 执行顺序：

1. 读取一个冻结 focus page；
2. `submit_change_decision` 选择 edit、resume_candidate、need_context 或安全退出；
3. need_context 用 path/offset/limit 增加不重复页；
4. 每个 edit_file 目标必须已进入 focus context；
5. edit/resume 后运行冻结 targeted CheckContract；
6. 普通业务轮最终仍需 verification check 与 TaskOutcome v3 candidate freeze。

v4 的完整文件 EvidenceContract 保持历史语义；v5 禁止为“完整覆盖”顺序翻遍文件。

## 验证

- DefinitionCompiler 与 Graph validation 覆盖 v3 拒绝、v4 唯一 repair producer；
- L3 单测覆盖 focus page、同文件 offset、未读 edit target、resume_candidate 与 check；
- fake-provider 真实二进制验证 Explorer 路由、v5 gate、repair mutation/check、
  Acceptance、Delivery promotion 和 active reservation=0；
- 真实 Qwen 定向题验证 architecture/model-contract 均通过；复杂题业务仍受
  execution window 与模型补丁收敛能力限制。
