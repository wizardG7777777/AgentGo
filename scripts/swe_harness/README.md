# AgentGo SWE Harness

该目录是 Flask SWE 系统回归的受版本控制契约。`/tmp/agentgo-swe` 只保存题目
metadata、prompts、隔离 worktree 与原始运行产物；不得再维护另一份 `run_task.sh`
或终态判断逻辑。

环境变量：

- `AGENTGO_SWE_API_KEY`：外部 provider 密钥，仅进程环境读取；
- `SWE_BASE_URL` / `SWE_MODEL`：统一 provider/model；
- `SWE_TESTBED`：默认 `/tmp/agentgo-swe`；
- `SWE_TASKS_FILE` / `SWE_PROMPT_DIR` / `SWE_FLASK_REPO`：外部题目数据位置。

单题：

```bash
bash scripts/swe_harness/prepare_task.sh automatic-options
bash scripts/swe_harness/run_task.sh automatic-options 1200
bash scripts/swe_harness/judge.sh automatic-options
```

八题：

```bash
bash scripts/swe_harness/run_all.sh 1200
```

`result.json` 使用 `agentgo.swe-result/v2`，分别报告 `architecture_ok` 与
`task_resolved`；禁止用 pytest 偶然通过覆盖架构事故，也禁止用模型业务失误伪造
框架错误。
