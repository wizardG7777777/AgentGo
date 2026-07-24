# 测试提示词 2：落盘产物 + 文件/命令证据验收

> 用途：回归检查「验收证据硬规则」（command 逐字匹配、file_hash 一致、task_status 裸
> 状态词）与「同因失败熔断」。verifier 需要真实执行命令并引用证据，任何格式退化都应
> 被快速熔断而不是无限重试。

## 提示词原文（直接粘贴给 AgentGo）

```text
在项目里做一次健康检查并落盘：创建 docs/health-check.md，内容包含你实际运行过的
3 个检查命令（如 go build、go vet、git status）的真实结果摘要。完成后走正式验收，
验收标准要覆盖：文件存在且内容非空、go build 通过、git 工作区无异常。
```

## 预期行为

- verifier 真实执行 `go build` 等命令后逐字引用命令串提交证据；验收 verdict=pass。
- 若验收 fail，reason 应给出可操作的修正指引（如 `output must exactly equal the bare
  status word`），且同一类格式失败连续 2 次后 Plan 被熔断挂起
  （`acceptance_circuit_open`），而不是反复重开验收。

## 检查方法

```bash
# 验收事件的 verdict / reason / 指纹
grep acceptance_completed .agentgo/sessions/<sess>/logs/*.jsonl | grep -oE '"verdict":"[a-z]+"|"reason":"[^"]{0,120}'

# 熔断触发时 scheduler 会看到 ErrAcceptanceCircuitOpen，Plan 进入 paused
./agentgo trace plan <plan-id>
```

## 异常信号（说明回归了）

- 出现 5/5 PASS 但 verdict=fail 且 reason 不含修正指引 → 核验错误文本退化；
- 同一类证据失败连续 3 次以上仍在重开验收 → 熔断失效（检查
  `internal/plan/acceptance.go` 的 `leadingExternalFactFailures` 调用是否还在）。
