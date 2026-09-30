# Core-96 原生 SWE 扩展实施记录

状态：题库与 Runner 接入已完成。2026-09-24 的 Core-96 双平台评测已结算 192/192 项，177 项修复成功、15 项失败。成绩累计自多个冻结批次及不同 Runner/AgentGo 二进制，不代表单一版本统一重跑或全数通过；逐次报告和原始证据保存在本地 testbed，不纳入 Git。

截至 2026-09-25，旧评测目标已停止，受管现场清理完成；后续评测应使用新的冻结尝试和独立运行目录。本文描述题库范围、执行边界与验收规则，不表示当前存在运行中的批次。

## 固定范围

第一大类为 12 个仓库，每个仓库 8 道独立真实修复题，共 96 题：

| 套件 | 上游 | 执行与判题方向 |
|---|---|---|
| gin-8 | gin-gonic/gin | Go JSON，HTTP/路由/上下文/绑定 |
| cobra-8 | spf13/cobra | Go JSON，命令/参数/补全 |
| bbolt-8 | etcd-io/bbolt | Go JSON，事务/页面/游标/存储 |
| gorm-8 | go-gorm/gorm | Go JSON，根模块与 tests 模块、SQLite |
| sympy-8 | sympy/sympy | pytest，符号计算与数学语义 |
| zod-8 | colinhacks/zod | Vitest，校验与类型相关行为 |
| vue-8 | vuejs/core | Vitest，响应式/组件/编译 |
| excalidraw-8 | excalidraw/excalidraw | Vitest，编辑状态/图形/交互 |
| tokio-8 | tokio-rs/tokio | Cargo/libtest，异步/资源/调度 |
| ripgrep-8 | BurntSushi/ripgrep | Cargo/libtest，搜索/文本/文件系统 |
| fmt-8 | fmtlib/fmt | CMake/CTest，格式化/模板/边界 |
| pytest-8 | pytest-dev/pytest | pytest，收集/fixture/插件/报告 |

Flask-8 保留原默认入口与既有题库。Gin-1 的历史证据保留；其已验证题目纳入 Gin-8 时不重复计入 Core-96。
不修改或验证 Docker 流程。PowerShell 在 Windows 原生运行；zsh 在 Linux 用户空间运行，不能据此声称 macOS 原生通过。

## 接入与题目要求

1. 所有套件仍由 `scripts/swe_test_runner/runner.py` 的 probe/task/batch/verify-candidates 进入。
2. 每题记录上游、问题说明、修复提交、基线提交、测试补丁路径、源码补丁路径、精确工具链和测试范围。
3. 题目来自真实修复；不复制题目凑数，不把多个测试函数当成多道题，不把未验证候选计为正式就绪。
4. 每个仓库尽量覆盖不同模块和故障类型。正确源码补丁仅供题目资格验证，不能交给修复 Agent。
5. 每题在两个原生环境核对：干净基线、仅测试补丁的红态、正确源码补丁后的目标通过与回归保持。
6. 回归范围提前声明并在全部阶段冻结。大型项目可采用明确的受影响模块回归，不能把模块范围描述成整个仓库测试全通过。
7. 目标失败必须修复；原通过项仍通过，不能新增失败、减少测试、通过跳过测试取得成功或篡改测试/依赖配置。
8. 提供共同的测试执行身份与逐项结果，同时保留各测试框架的原始输出。编译失败、零测试、截断输出、缺少终态不能伪装成通过。
9. 真实模型验证与题目红绿验证分别记账；Graph/Delivery/TaskOutcome、模型/工具调用、测试结果和未结算事实分别核对。
10. 完整证据、工具链、上游仓库和临时工作树保存到外部 testbed；不提交个人配置、凭据或原始模型日志。

## 实施顺序

- 核查并下载 12 个仓库，建立候选修复清单。
- 增加版本化套件/题目配置与公共原生事务逻辑，保留已有 Flask/Gin 接缝。
- 实现 Go 多模块、pytest、Vitest、Cargo/libtest、CTest 的环境与结果适配。
- 逐题完成双 Shell 红绿资格验证；失败题修整环境或以同仓库其他真实修复替换，直到每仓库恰好 8 题有效。
- 运行真实模型验收并读取证据，整理公开报告与未完成项。

已建立 12 个 suite/v3 套件和 core-96 组入口，共 96 道题；Runner 提供 Go/pytest/Vitest/Cargo/CTest 适配与结果归一化。双平台完整评测已完成结算，未解决题目仍按失败成绩保留。题库定义在仓库中，逐次资格/模型结果、归档及运行现场留在本地 testbed。

已确认真实模型验收范围为 96 题 × PowerShell/zsh，共 192 次运行。每个仓库的 8 题须先通过双端资格验证并冻结，再进入该仓库的 16 次真实模型运行。已冻结仓库与其余仓库的资格验证可以并行；不因模型修复失败更换考题。
