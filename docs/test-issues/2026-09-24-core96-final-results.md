# Core-96 双端 SWE 最终结果：192 项评测结束

> 后续资源状态（2026-09-25）：用户已授权清理全部保留现场。下文记录的是 9 月 24 日核对时的历史状态；当时 14 个保留现场及两处未封存现场均已在后续操作中清理，两处中断现场先补封存再删除。原成绩不变。

**评测于 2026-09-24 20:07:11 +08 正常结束，192/192 项均已有最终成绩；177 项修复成功，15 项失败，成功率 92.2%。** r43 控制器已退出，完成回执 complete=true、missing 为空。评测完成不表示全部修复成功，也不表示所有历史失败现场已删除。

## 范围与成绩

| 平台 | 已评测 | 修复成功 | 失败 |
|---|---:|---:|---:|
| Windows / PowerShell | 96 | 87 | 9 |
| Linux / WSL Ubuntu zsh | 96 | 90 | 6 |
| 合计 | 192 | 177 | 15 |

macOS 按用户先前要求延期，不计入本轮。以下为每项目 8 题的成功数；每个平台均已完成全部 8 项评测。

| 项目 | Linux 成功 / 8 | Windows 成功 / 8 |
|---|---:|---:|
| Gin | 8 | 8 |
| Cobra | 8 | 8 |
| bbolt | 8 | 7 |
| GORM | 7 | 7 |
| SymPy | 8 | 8 |
| Zod | 8 | 7 |
| Vue | 7 | 7 |
| Excalidraw | 7 | 6 |
| Tokio | 8 | 8 |
| ripgrep | 7 | 6 |
| fmt | 6 | 7 |
| pytest | 8 | 8 |

本结果按当前 r43 题库的 192 个唯一平台/题目身份统计，从 31 份覆盖来源与初始 53 项证据索引追溯逐题记录。保留合法原补丁复判结果，不重复计算模型调用或覆盖；退役 ripgrep `0407e104f611` 的旧 Linux 成绩不计入当前题库，新题 `d199058e77cb` 已在两端真实评测成功。历史失败、错误二进制运行和未计分的基础设施中断仍保留，不移植成绩。

这是多个冻结批次累计完成的基线，包含不同历史 Runner/二进制身份及明确的复判，不应描述为最新单一版本从零统一重跑 192 项。

## 最后一批与最后一题

r43 新增 22 项真实评测：20 项成功，2 项 deadline_failure。此前其余 170 项与这些结果合并，构成上述 192 项。资格验证和网络夹具不计入模型成绩。

最后一题 `pytest-977575388358`（Windows）Judge 为 resolved，286 项通过、0 失败，未检测到测试篡改。27 次模型调用全部结算，execution_complete / settlement_verified 为 true，无外部强杀、无待交付结果、无活动 reservation；随后归档、清理与控制器审计完成才计入第 192 项。

最终控制目录：`%LOCALAPPDATA%/AgentGo/swe-resource-validation-20260924-r43/continuation-20260924`。权威文件为 `completion.json`、`coverage.json`、逐题 `*-audit.json`，最后一题为 `041-windows-pytest-977575388358-audit.json`。

## 失败清单

13 项明确标记为 deadline_failure；其余两项保留既有失败判读。独立 Judge 的最终分布为 resolved 177、failed 14、test_tampered 1。失败不是未执行，也没有用重复模型调用覆盖这些成绩。

后续逐项日志复核发现，deadline 是停止条件而非完整根因：Windows 7 项与 Linux 4 项的最终直接阻塞为 AgentGo 内部 Shell 授权；GORM 另有授权等待及 go.sum 输入变化，Windows Excalidraw 还有一次本地上下文装配拒绝。Linux GORM 虽未列为 deadline_failure，也达到 1,200 秒。原成绩不改写。

| 平台 | 题目 | 记录类别 |
|---|---|---|
| Windows | `bbolt-49c76973447c` | deadline_failure |
| Windows | `gorm-7b1fb0bd7324` | deadline_failure |
| Windows | `zod-cafbee4772ef` | deadline_failure |
| Windows | `vue-c04a45d15498` | deadline_failure |
| Windows | `excalidraw-ab0255f21eb4` | deadline_failure |
| Windows | `excalidraw-53732f08f430` | deadline_failure |
| Windows | `ripgrep-b610d1cb1506` | deadline_failure |
| Windows | `ripgrep-4ab1862dc047` | deadline_failure |
| Windows | `fmt-6aa4a5f6f8c2` | deadline_failure |
| Linux | `excalidraw-2b1c2a9fbf62` | deadline_failure |
| Linux | `ripgrep-4df129812769` | deadline_failure |
| Linux | `fmt-7852fc384ce4` | deadline_failure |
| Linux | `fmt-2012961e3ec6` | deadline_failure |
| Linux | `vue-378f9783248a` | 历史 Judge failed |
| Linux | `gorm-61520d7476a5` | 历史失败，Judge test_tampered |

## 资源收尾核对及尚存边界

本次只读核对 192 个选定结果的当前 attempt、归档清单/压缩包大小、Judge 与清理回执，均无读取缺项，Judge 与选定成绩一致。全部 quiescent=true、cleanup_complete=true，无 resource_error / finalization_error，未发现这些选定现场残留私有配置。

| 选定结果现场 | Windows | Linux | 合计 |
|---|---:|---:|---:|
| 原始工作树/scratch/commands 已清理 | 87 | 91 | 178 |
| 按保留策略留存原始现场 | 9 | 5 | 14 |

完整 ZIP 内容校验采用各次收尾已有的独立审计证据；本次复核当前归档元数据和文件大小，没有重新解压/散列全部历史归档。14 个保留现场已有归档与清理回执，并非失控删除失败。

r43 防火墙助手 closed，授权申请 **1502**、释放 **1502**、剩余 **0**，operation_failures 为空，受保护目录已删除。

另对主评测根与复判根的历史 attempt 元数据做了只读盘点：Windows 主根有 20 个保留/未完成现场，Linux 主根 15 个，两端复判根无额外保留。该计数包含不计入当前 192 项的历史中断，不能与上表重复相加作为评测数。

**仍有两个 Windows 历史中断现场待封存与后续清理，不属于运行中的评测：**

1. r36 Tokio 模型尝试 `3404dea31a624293b6a509f3cbbe0385`，批次 `20260923T081037Z-4cc89cfdb1aa`。旧状态 retained / quiescent=false，记录 process_quiescence_unknown，尚无归档/清理回执；`reports/setting.yaml` 仍存在。本次仅检查存在性，未读取配置或密钥。
2. 中断网络夹具 `2c3b58226a8246c08c9b74c65ebf1e33`，批次 `20260923T180826Z-6179ebc03f87`，旧收尾记录 KeyboardInterrupt，尚无归档/清理回执，无私有配置。

对上述两处记录的 2237 / 16 个进程身份及扫描器、scoped 子执行进行了只读复核，未发现仍存活的记录身份。本次没有手改历史 quiescent、删除文件或启动模型。**因此评测执行已全部完成，但不能宣布历史资源收尾全部完成。**

## 本次汇总产物

本地忽略目录 `artifacts/swe-final-20260924/` 保存：

- `lineage.json`、`summary.json`：完整逐题来源、当前成绩、项目/平台汇总与失败集合。
- `resources-windows.json`、`resources-linux.json`：192 个选定现场的当前元数据核对。
- `history-windows.json`、`history-linux.json`、`unsealed-process-review.json`：历史保留及未封存现场边界。

这些脚本只读取证据并写独立报告，没有重新运行评测、调用 Provider、改变历史成绩或清理现场。部分历史行保留旧 runtime 标签，已按精确 attempt_id 找到实际审计文件，不凭标签猜测结果。
