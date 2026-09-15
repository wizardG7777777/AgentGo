# 2026-09-12 SWE_Linux 默认清理与持久化旗标

状态：实现并实际验证。默认仅导出报告/行为日志，导出校验通过后销毁本批容器及独立数据卷；
`--persist` 保留停止状态的容器和完整内部现场。未使用真实 API 凭据或运行真实 provider SWE。

后续已改为独立的 Flask 系列镜像及外部 AgentGo 二进制输入。本报告中的旧镜像名、--no-build 命令
记录该阶段的历史验证；当前入口见 [使用说明](../../SWE_LinuxContainers/README.md)，清理与 --persist 语义保持不变。

## 当前契约

- CLI：`py -3.13 SWE_Linux/run.py run --no-build` 默认清理；追加 `--persist` 保留。
- 每批使用单独的容器和 `agentgo-swe-linux-data-<batch-id>` 卷，均标记批次归属。
- 宿主复核 `agentgo.swe-linux-export/v2` / `reports-and-behavior` 的批次身份与文件摘要，
  然后检查容器停止和资源归属，依次删除本批容器、数据卷。未使用 prune 或强制删除。
- 测试失败也执行默认清理，前提仍是报告/日志已完整导出；导出失败时保留尚存现场并返回错误。
- 持久化只改变资源保留方式，导出范围仍然是报告/行为日志。
- 取消默认完整 evidence.tar.gz，以及独立配置/环境、源码包、Git/.venv/候选目录、setting.yaml、model.patch 导出。
  Graph/Context/Loop/Outcome、会话、Trace、Prompt dump 和引用的 content 正文保留。
- 宿主源码、`.build` 构建暂存产物、镜像以及历史批次不删除。
- 镜像含导出契约标签；旧版镜像必须重建，不能用旧的完整现场归档回执触发新清理逻辑。

## 实际验证

镜像 ID：`sha256:41baa42dc788e348a110665e26200db0fc6208f4ed86cfa75397ec47b4f0c91c`。
验证材料：`${LOCALAPPDATA}/AgentGo/swe-linux-lifecycle-validation-20260912/`。
关键文件为 build.log、lifecycle-results.json 和 artifacts 下各批的 archive-status.json / cleanup.json。

| 场景 | 批次 | 退出码 | 容器/卷结果 | 导出 |
|---|---|---:|---|---:|
| 空配置，默认 | 20260912T114248Z-1356f57e | 1 | 两者已删除 | 3 个报告文件 |
| 空配置，--persist | 20260912T114250Z-b7ae4f31 | 1 | 两者保留 | 3 个报告文件 |
| Flask-8 verify-candidates，默认 | 20260912T114251Z-601f9ea2 | 0 | 两者已删除 | 102 个报告/日志文件 |

以上额外的 host/container-state/cleanup 回执不计入容器导出文件数。空配置在启动后解释失败，
证明“失败测试也清理”以及“存在性门禁不预先拒绝空白”同时成立。
八题自检全部完成干净基线、golden tests 红态与 golden fix 绿态，不是模型修复成绩。
旧共享卷 `agentgo-swe-linux-data` 仍在，未被本批清理波及。

真实 AgentGo Linux 二进制配合本地 Responses SSE fixture 完成 27 次调用、增量图交付成功。
其行为记录导出 51 个文件、5238734 bytes，已校验 ContextSnapshot、Session 模型回复、
Prompt dump 和 Trace 都存在，未导出候选目录或完整源码包。该辅助容器使用 --rm，结束后删除。

其他验证：

- Windows 和 Ubuntu 24.04 容器内封装回归各 16 项通过。
- 原 Runner Python 回归 70 项通过。
- go test ./...、go vet ./...、go build 通过。
- 导出损坏禁止清理、共享/外来卷保护、运行中容器拒绝删除、部分清理失败回执、
  符号链接越界及日志正文保留由确定性测试覆盖。
- git diff --check 通过。

## 文件对账与边界

新增 export_logs.py。修改 run.py、entrypoint.py、Dockerfile、Compose、封装测试与使用说明；
更新 Runner 文档和 KNOWN_ISSUES。未修改生产 L1/Graph/SWE 判题契约，不迁移历史数据。

`--persist` 保留停止状态的容器，不自动续跑旧批次。宿主自身崩溃、Docker daemon 不可用及
真实 OOM 的自动恢复没有在本轮制造实测；这类未完成动作保留原始现场，不伪造清理或测试成功。
报告中的完整行为正文仍可能包含源码或敏感内容，导出范围收窄不等于正文脱敏。
