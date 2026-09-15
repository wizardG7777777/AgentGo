# 2026-09-15 SWE 容器源码交付与隐私边界

本次整理容器源码及其必要依赖，不重新运行真实 provider SWE，不更新已发布镜像。
既有真实模型成绩仍以 [2026-09-13 Docker 报告](2026-09-13-docker-flask8-real.md) 为准。

## 文件与行为对账

- 新增 `SWE_LinuxContainers/`：共享镜像制作、外部 Linux ELF 输入、预制依赖、报告与行为日志导出、默认清理和 `--persist`。
- 新增 Runner 预制环境安装与探针原文归档模块；同步角色/题目 Prompt 的直接虚拟环境 Python 命令。
- 新增 `AGENTGO_TRACE_KEEP_ALL=1`，使容器归档前保留完整 Trace；初始启动和 Session 重绑均使用相同策略，普通运行仍保留 100 个文件。
- 修正容器改名后的构建资源路径、CI 和当前文档链接；公开模板统一命名为 `config.json.example`。
- 更新 `AGENTS.md`、Git/Docker 忽略规则与构建源码清单。个人配置、环境文件、日志、工作区、构建暂存、镜像归档和二进制不提交。
- 四份历史容器报告移除个人绝对路径，保留原批次、镜像与二进制身份。历史命令仅作为历史记录，当前入口使用新目录。
- 无历史数据迁移或清理；其他工作区文档修改、删除和历史 stash 不纳入本次提交。

## 本次验证

验证使用从暂存内容创建的独立检出目录；容器挂载由源码清单生成的测试资源和本次构建的 Linux 二进制。

| 检查 | 结果 |
|---|---|
| Windows 容器封装回归 | 24 项通过 |
| Windows Runner 回归 | 70 项通过 |
| Ubuntu amd64 容器封装回归 | 24 项通过 |
| Ubuntu amd64 Runner 回归 | 70 项，按平台跳过 1 项，其余通过 |
| Windows Go 全量测试、vet、构建 | 通过；本地 Go 1.26.0 |
| Linux amd64 交叉构建 | 通过；CGO_ENABLED=0 |
| Windows Responses / Chat Completions 二进制冒烟 | 两协议均 Graph success，各 27 次本地 fixture 调用 |
| Linux Responses / Chat Completions 二进制冒烟 | 两协议均 Graph success，各 27 次本地 fixture 调用 |
| 配置与构建上下文隔离 | 真实配置路径被忽略；公开模板仅含空占位值；源码旁私有文件不进入构建上下文 |
| 公共镜像索引 | 核对存在 linux/amd64 和 linux/arm64，详见容器 README |

容器测试使用 `--network none`，未挂载个人配置；没有真实模型调用。
Linux CI 的 race 检查补充 Bootstrap/Trace；本次本地环境未运行 race。
以上不代表重新构建、发布完整依赖镜像，也不代表 macOS 原生、arm64 真实模型或新的 Flask-8 修复成绩。
