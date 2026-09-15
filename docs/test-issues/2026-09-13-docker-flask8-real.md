# 2026-09-13 Docker Linux amd64 真实 Flask-8

状态：**8/8 完成、8/8 真实模型修复成功，退出码 0**。报告和行为日志已导出并逐文件复核，
本批容器及独立数据卷已按用户要求删除。未读取或展示用户 config.json 正文、密钥真值。

## 执行身份

- 本地时间：2026-09-13 03:33:32 至 03:56:21 AWST，容器运行约 **22 分 49 秒**。
- 批次 ID 使用 UTC：`20260912T193327Z-63b12597`，不是复用旧批次。
- 用户已将宿主目录改名为 `SWE_LinuxContainers`，本次使用新目录中的启动器和个人配置。
- 命令：`py -3.13 -u SWE_LinuxContainers/run.py --agentgo-binary "$env:LOCALAPPDATA/AgentGo/swe-flask-image-validation-20260912/agentgo-linux-amd64"`。
- 未启用 --persist；容器 `swe-linux-20260912t193327z-63b12597`。
- 镜像：`agentgo-swe-flask:ubuntu24.04-py3.13-v1`，索引 `sha256:dead0a23264c8890bc2a63d9a69d75163b353ff769dfa93524ed48d5fefdb710`。
- Linux 二进制 SHA256：`665dde08285c4c2b6c79f9c88a6fb6745c1d35c841c97a79ff52f564861eb038`。
- Ubuntu 24.04 / linux/amd64，Docker Desktop WSL2 Linux 内核；Python 3.13.5、pip 预装环境。
- 真实 Responses SSE，各题结果中的 fast/flag_ship 映射均与此前 Windows 基线一致。
- 批次期间未修改镜像、二进制或 Prompt，未给测试 Agent 提供修复建议；外部观察只读取运行证据。

## 逐题结果

| 题目 | Windows → Docker | Docker 调用 | AgentGo 秒数 | pytest passed / failed / skipped | 补丁行数 |
|---|---|---:|---:|---|---:|
| automatic-options | 成功 → 成功 | 16 | 56 | 494 / 0 / 0 | 21 |
| context-push-order | 成功 → 成功 | 32 | 131 | 487 / 0 / 2 | 17 |
| ipv6-server-name | 成功 → 成功 | 15 | 47 | 493 / 0 / 0 | 19 |
| ipv6-session-txn | 成功 → 成功 | 33 | 107 | 492 / 0 / 0 | 13 |
| pass-context-dispatch | 成功 → 成功 | 82 | 348 | 490 / 0 / 0 | 429 |
| secret-key-rotation | 成功 → 成功 | 20 | 74 | 487 / 0 / 2 | 23 |
| session-access-tracking | 成功 → 成功 | 53 | 250 | 490 / 0 / 0 | 156 |
| teardown-callbacks | 成功 → 成功 | 34 | 280 | 495 / 0 / 0 | 154 |

每题 errors=0、tampered=false，正式判题相对于本题基线的 added_failures 为空。
测试前后输入 digest 相同，且与 judge.test_input_digest 一致；Run 身份对应。
正式 pytest 的 cwd 和 Flask 实际导入路径指向本题主 worktree 的 src/flask。

## 与 Windows 基线的比较

对照来自 [2026-09-12 Windows 真实批次](2026-09-12-flask8-after-context-and-l3-fixes.md)。

| 指标 | Windows | Docker |
|---|---:|---:|
| 修复成功 | 8/8 | 8/8 |
| 已结算业务模型调用 | 335 | 285 |
| AgentGo 秒数合计 | 1554 | 1293 |
| prompt tokens | 11,668,964 | 16,537,718 |
| completion tokens | 240,119 | 184,521 |

业务调用不含探针。本轮另有 1 次 Python 前置探针、8 次 AgentGo 启动探针，均完成。
没有观察到 provider HTTP 错误，没有从 token 数或 cost_micros 推算真实账单。

成功率和最终测试结果与 Windows 达到同样效果，但效率指标并非全部下降。
最后一题累计输入 9,858,531 tokens，单次最大 provider prompt_tokens 为 613,195；
其调用次数更少，耗时反而从 Windows 的 168 秒变为 280 秒。
这不是严格单变量的系统性能实验：Linux 使用 pip 预装环境及直接虚拟环境 Python 命令，
历史 Windows Prompt 使用 uv 命令；服务端采样、缓存和模型后端也没有冻结。
因此不能把调用量、token 和耗时差异全部归因于 Docker/Linux。

## 独立核验与残余工具错误

- 八题 architecture_ok、execution_complete、model_contract_compatible、infrastructure_ok 均为 true。
- 285 个业务 ContextSnapshot 对应 285 个 model-output，全部采用 context:default/v12。
- 八张图的完成意图均为 committed，节点均为 agentTask；settlement_verified=true。
- 无未结算调用、external_hard_kill 或 OOM，无新增测试失败。
- 有 **41 条去重后的 tool_result.error**：run_shell 26、control_graph 7、submit_task_result 6、
  apply_change 1、read_file 1。其中 25 条为 pipeline scope 问题，6 条为未知结构化参数。
  模型纠正后继续完成，不能将 8/8 解读为零工具误用。

外部观察检查 Python 探针、启动探针与 Trace 的 HTTP 错误，发现后会请求停止本批。
本轮未触发该路径，不能由零 HTTP 错误推导所有故障/重试边界已验证。

## 报告、日志与清理

主归档：

```text
SWE_LinuxContainers/artifacts/20260912T193327Z-63b12597/
  runs/summary.json
  runs/<题目>/result.json
  runs/<题目>/judge.json
  runs/<题目>/judge.pytest.execution.json
  behavior/<题目>/
  provider-probes/
  archive-status.json
  container-state.json
  cleanup.json
```

导出清单包含 516 个文件、168649467 bytes，全部摘要复核通过。
cleanup.json 记录 persist=false、container_removed=true、volume_removed=true；
Docker 当前查询也确认本批容器和卷不存在。其他历史验证容器不属于本次清理范围。

外部观察与独立核验记录位于 `${LOCALAPPDATA}/AgentGo/swe-docker-launches/`：
`launch-20260913-033326.log`、`20260912T193327Z-63b12597-watch.jsonl`、
`20260912T193327Z-63b12597-verification.json`、`20260912T193327Z-63b12597-diagnostics.json`。
观察/核验记录独立存放，不改变容器导出清单。

个人配置由容器入口读取，未向聊天输出内容或密钥；默认导出不包含独立配置/环境快照。
本轮新增报告、更新 KNOWN_ISSUES、补齐改名后目录的 .gitignore；无受测程序或执行契约改动，
无提交/推送、无历史数据迁移或删除。
Linux arm64 真实 provider、M3 Max 原生及真实 Chat Completions 完整批次仍须独立验证。
