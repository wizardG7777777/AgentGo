# 2026-09-12 SWE_Linux 容器构造与离线验证

状态：Ubuntu 24.04 容器已构建、实际启动并通过离线验证。**未执行真实 provider 的
Linux Flask-8 修复批次**。本报告的八题通过指题目红绿自检，不是模型修复成绩。

本报告记录初始“保留容器并导出完整现场”的历史版本。当前已更新为默认仅导出报告与行为日志，
随后销毁本批容器/独立数据卷，`--persist` 显式保留；以 [当前说明](../../SWE_LinuxContainers/README.md) 为准。

## 配置与构建契约

- 公开模板 `SWE_Linux/comfig.json.example`；个人真实文件 `SWE_Linux/config.json`。
- 官方入口只检查文件存在，缺失时在 Go/Docker 工作之前失败。构建不解析内容；
  空字节文件已实际完成镜像构建和容器启动，随后自然出现 JSONDecodeError 并归档。
- 宿主 Go 1.25.0 交叉编译 linux/amd64、CGO_ENABLED=0；目标容器接收 ELF，未安装 Go/GCC。
- 最终用户空间实测 Ubuntu 24.04.4 LTS，Python 3.13.5、uv 0.8.19。
- 内核实测 `6.6.87.2-microsoft-standard-WSL2`，来自 Docker Desktop 的 WSL2 后端。
- 原有 Runner 的四变量契约与前置探针保留，不新增 L1 负向测试入口。

## 执行身份

- 最终本地镜像：`agentgo-swe-linux:local`。
- 最终镜像 ID：`sha256:12617a868c12e1265d6e42af5890fc020446dd6008600276acd19a89b919b6ba`。
- 最终 Linux 二进制 SHA256：`665dde08285c4c2b6c79f9c88a6fb6745c1d35c841c97a79ff52f564861eb038`。
- 首次八题自检镜像 ID：`sha256:729fb391914c93d1a5b9bba86bbca74fecd2d1d34b80becd134dbe6344ad8b0c`。
  后续补齐异常归档恢复与宿主元数据的归档副本；最终镜像重新验证空配置执行和 9 项封装回归。
- 每次构建的 `build.json` 保存逐文件源码摘要、实际 Go 工具链、目标架构和二进制摘要；
  `source.tar.gz` 保存实际源码，不把单独的 Git HEAD 当作全部受测身份。

本机验证材料位于：

```text
${LOCALAPPDATA}/AgentGo/swe-linux-validation-20260912/
  final-build.log
  artifacts/verify-fixture-20260912/
  artifacts/probe-fixture-responses/
  artifacts/probe-fixture-chat-completions/
  artifacts/20260912T101436Z-119346c1/
```

所有验证配置均是空文件或本次生成的离线 fixture；没有读取现有真实凭据，没有创建仓库内的个人 config.json。

## 验证结果

| 验证 | 结果 |
|---|---|
| Windows `go test ./...`、`go vet ./...`、`go build` | 通过，宿主默认 Go 1.26.0 |
| Windows 原 Runner Python 回归 | 70 项通过 |
| SWE_Linux 封装回归 | Windows / 最终 Linux 镜像各 9 项通过 |
| Trace 全保留 | 超过 100 个文件、Writer 重绑后的历史保留验证通过；默认仍保持 100 上限 |
| Windows 与 Linux 的双协议真实二进制 fake-provider 冒烟 | 均通过；Linux 两协议各 27 次本地模型调用，Graph success |
| 容器内 Python 双协议前置探针与原文归档 | 均通过；相同能力模型去重为每协议一次调用 |
| 容器内 Flask-8 `verify-candidates` | 8/8 干净基线、golden tests 红态、golden source fix 绿态成立 |
| 空文件与错误 JSON | 接受文件存在，运行时解释失败，原始字节及错误阶段归档 |
| 缺失 config.json | 官方构建入口停止；没有自动创建空配置 |
| Git 忽略边界 | 真实 config、构建上下文、产物被忽略；公开模板可跟踪 |

Linux 二进制冒烟、题目自检和探针验证均使用 `--network none`；本地 HTTP fixture 仅使用容器回环。
预制 Python 环境通过复制安装，正式题目自检期间不执行 uv sync、依赖构建或 Go 构建。

八题 golden fix 最终测试：

| 题目 | passed | failed / errors | skipped |
|---|---:|---|---:|
| automatic-options | 494 | 0 / 0 | 0 |
| context-push-order | 487 | 0 / 0 | 2 |
| ipv6-server-name | 493 | 0 / 0 | 0 |
| ipv6-session-txn | 492 | 0 / 0 | 0 |
| pass-context-dispatch | 490 | 0 / 0 | 0 |
| secret-key-rotation | 487 | 0 / 0 | 2 |
| session-access-tracking | 490 | 0 / 0 | 0 |
| teardown-callbacks | 495 | 0 / 0 | 0 |

自检容器退出码 0，OOMKilled=false。该批 `evidence.tar.gz` 为 138123810 bytes，
SHA256 `9d23240dcbbdd1a1137ffe761944b3a68e255044fac3a7531892b76ee63d8304`，
已重读压缩流验证。各题 pytest 原文、阶段报告、执行身份及被测工作树保留。

## 文件对账与限制

新增宿主构建/运行入口、Dockerfile/Compose、明文配置模板、依赖准备、容器监督与归档和回归。
Runner 新增可选预制依赖安装与 Python probe 原文记录；Bootstrap 新增显式 Trace 全保留开关。
使用说明、KNOWN_ISSUES 与现有 Windows/Linux/macOS CI 同步更新。删除/迁移为零。

本次没有移植历史状态、修改 L1 协议/Graph 判题或启用新的 L1 测试模式。
没有声称验证实际 OOM、宿主掉电或完整真实 SWE；归档 I/O 失败和保留现场由确定性回归覆盖。
业务请求与输出依赖现有 ContextSnapshot、会话及 Trace 原文，不等同于 L1 网络字节录制。
CGO/race、arm64 原生运行和真实 provider Linux 修复成功率不在本次已验证范围内。
