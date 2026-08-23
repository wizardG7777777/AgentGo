# AgentGo SWE Harness

该目录是 Flask SWE 系统回归的受版本控制契约。所有测试入口、进程编排、判题和
汇总逻辑统一由 Python CLI `harness.py` 执行；仓库及 `/tmp/agentgo-swe` 均不得
维护 Bash 测试脚本。`/tmp/agentgo-swe` 只保存题目 metadata、prompts、隔离
worktree 与原始运行产物。

环境变量：

- `SWE_API_KEY`：外部 provider 密钥，仅进程环境读取（固定变量名）；
- `SWE_BASE_URL` / `SWE_MODEL` / `SWE_PROTOCOL`：统一 provider/model/协议；
- `SWE_TESTBED`：默认 `/tmp/agentgo-swe`；
- `SWE_TASKS_FILE` / `SWE_PROMPT_DIR` / `SWE_FLASK_REPO`：外部题目数据位置；
- `SWE_AGENTGO_ROOT` / `SWE_AGENTGO_BIN`：AgentGo 仓库根与二进制位置（默认取仓库内构建产物）。

当前 SWE 全局契约固定 `reasoning_effort=low`（thinking 仍开启），双层能力探针与
Scheduler/Proposal 机械阶段使用 `tool_choice=auto` + singleton ToolRouter +
L3 required-action gate。Graph 最终交付是狭义例外：历史投影收窄后，仅该次
Invocation 覆盖为 `reasoning_effort=none` + exact `submit_task_result`；不按
provider/model 名称分支，其余业务推理仍走 thinking。

## 启动方式

完整执行一道题（能力探针 → prepare → run → judge）：

```console
python3 scripts/swe_harness/harness.py task automatic-options --timeout 1200
```

执行 `tasks.csv` 中的完整批次：

```console
python3 scripts/swe_harness/harness.py batch --timeout 1200
```

独立运行能力探针：

```console
python3 scripts/swe_harness/harness.py probe
```

`prepare`、`run`、`judge` 是 `task` / `batch` 内部的固定阶段，不提供独立 CLI
入口，避免绕过能力探针、终态采集或最终判题。

验证所有候选题目的“干净基线绿 → test patch 红 → source fix 绿”语义：

```console
python3 scripts/swe_harness/harness.py verify-candidates
```

运行 harness 单元测试：

```console
python3 -m unittest scripts/swe_harness/harness_test.py
```

`result.json` 使用 `agentgo.swe-result/v2`，分别报告 `architecture_ok` 与
`task_resolved`；禁止用 pytest 偶然通过覆盖架构事故，也禁止用模型业务失误伪造
框架错误。CLI 退出码为：`0` 表示所选测试全部通过，`1` 表示 harness/环境故障，
`2` 表示架构门失败，`3` 表示任务正确率门失败；批次不再在未达到目标时返回成功。
批次遇到架构门失败会立即停止，普通任务正确率失败则继续完成剩余题目并在汇总后
返回 `3`。
