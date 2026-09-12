# 2026-09-12 原文上下文与 L3/L5 修复后的完整 Flask-8

状态：正式批次 8/8 完成、8/8 修复成功，退出码 0。上一轮是 8/8 完成、6/8 成功。结果已独立重读日志、结算记录和测试身份核验，不能仅凭终端绿色摘要理解本结论。

## 1. 受测身份与执行边界

- 正式测试根：`C:/Users/73524/AppData/Local/AgentGo/swe-repairs-v7-batch-20260912-024222`。
- 时间：2026-09-12 02:43:19 至 03:11:07 AWST，约 27 分 48 秒。
- 命令：`py -3.13 -u scripts/swe_test_runner/runner.py batch --timeout 1200`；完整 Flask-8 `tasks.csv`，每题继续使用 1200 秒外部窗口。
- 源码 HEAD：`f5d513367c70ddbe100104457801d2bffbf0dea7` 加当前未提交修复，不能把 HEAD 当成全部受测源码。
- Windows 二进制 SHA256：`50781747C968824FD70D69B24F1F5990867317652C15C9AF3D32FA28EC493481`，测试结束后再次核对一致。
- Graph 为 `agentgo.graph/v7` / `graphs-v8`；Context policy 为 `context:default/v12`；SWE 上下文 880000 tokens。
- 真实协议为 Responses SSE。fast/flag_ship 两档均映射为 `deepseek-flash`，与上一轮结果记录一致。这里只证明配置名称一致，不宣称服务端权重或采样完全不变。
- 正式批次期间未修改受测程序或提示词，未向运行中的 Agent 提供修复建议。辅助 observe.py、compare.py、verify.py 只读取证据、写分析产物。

此前启动的 `swe-repairs-v7-batch-20260912-023301` 在第一题暴露 Git 长路径问题，已停止并保留诊断材料，不计入本表或正式总量。该问题属于上一轮 L3 实现遗漏：隔离 Git 配置后没有显式开启 core.longpaths。补修准备命令和副本配置，并通过 packed Git 路径超过 260 字节的回归后，才启动这里的完整批次。原始 diagnostic-stop.json、batch.log 和未结算现场保留，不包装成完整测试成绩。

## 2. 逐题结果与上一轮比较

| 题目 | 上轮 → 本轮 | 模型调用：上轮 → 本轮 | AgentGo 秒数：上轮 → 本轮 | 本轮 pytest passed / failed / skipped | 补丁行数 |
|---|---|---:|---:|---|---:|
| automatic-options | 成功 → 成功 | 139 → 26 | 516 → 122 | 494 / 0 / 0 | 21 |
| context-push-order | 成功 → 成功 | 84 → 45 | 401 → 275 | 487 / 0 / 2 | 21 |
| ipv6-server-name | 成功 → 成功 | 17 → 14 | 89 → 59 | 493 / 0 / 0 | 21 |
| ipv6-session-txn | 成功 → 成功 | 28 → 23 | 141 → 110 | 492 / 0 / 0 | 13 |
| pass-context-dispatch | 失败 → 成功 | 316 → 116 | 1201 → 452 | 490 / 0 / 0 | 435 |
| secret-key-rotation | 成功 → 成功 | 26 → 17 | 138 → 68 | 487 / 0 / 2 | 22 |
| session-access-tracking | 失败 → 成功 | 286 → 55 | 1200 → 300 | 490 / 0 / 0 | 149 |
| teardown-callbacks | 成功、超时停止标记 → 正常成功 | 281 → 39 | 1202 → 168 | 495 / 0 / 0 | 116 |

每题 `errors=0`、`tampered=false`，基线与最终判题的 collected nodeid 集合一致，新增失败事件集合均为空。各题使用各自的 Flask revision，不能把八题 passed 数相加解释为一套独立测试的覆盖数量。

## 3. 用量与效果

| 指标 | 上一轮 | 本轮 | 变化 |
|---|---:|---:|---:|
| 修复成功 | 6/8 | 8/8 | 两个未交付题均恢复 |
| 已结算业务模型调用 | 1177 | 335 | 减少 71.54% |
| AgentGo 阶段秒数合计 | 4888 | 1554 | 减少 68.21% |
| prompt tokens | 19,393,440 | 11,668,964 | 减少 39.83% |
| completion tokens | 783,785 | 240,119 | 减少 69.36% |

业务调用不含探针。AgentGo 秒数合计不包含所有准备和 Judge 时间；整批约 27 分 48 秒，上一轮约 83 分 25 秒。完整原文并不保证每题输入用量都下降：前四题合计输入从 3,767,574 增加到 4,944,698 tokens，但减少了大量调用；全批总输入仍下降。没有从 cost_micros=0 推导真实费用。

这里是同套题、相同配置模型名称的两次运行，不是逐请求固定采样的因果实验。日志能证明具体故障路径未复现和交付恢复；不能把全部耗时变化精确归功于某一个函数。

## 4. 原问题是否实际改善

### 4.1 工作基线与重复规划

生产 schema 和节点定义中的 workspace_input 已删除，Scheduler 不再填写基线槽。纯文本调查结果不充当代码版本；唯一候选继承，同谱系多个版本选择后继，独立分支明确拒绝任取。新目录和 Graph v7 阻止旧契约继续执行。

正式批次没有 workspace_input 调用，也没有 execution_blocked 或相同等待指纹重复发布。第五题的图日志从 235,810,870 bytes 降至 1,656,375 bytes，定义 revision 从 7 变为 2。检查节点曾正常等待实现结果，后续成功派发。因为本轮没有触发旧 invalid_input 场景，不能只用“零事件”证明去重边界；多个阻塞节点、规划确认、重启和原因变化由确定性回归另行覆盖。

### 4.2 原文历史与节点结束

八题 335 个 ContextSnapshot 全部使用 v12，记录的正文处理均为 inline，没有旧 fragment_limit_externalized 或局部分区预算拒绝。全量历史/大正文保真另有确定性测试；“没有拒绝”不等于本轮已触达模型整体 880K 窗口上限。

`ipv6-session-txn/fix-1` 实际使用普通文本结束，运行时原样登记 2626 字符的结论并保存 plain_text=true；候选仍经图级交付，最终 Judge 492 passed。其余 14 个业务节点采用结构化结果。没有恢复强制 Observation/record 或经验轮数关卡。

### 4.3 候选实际写回

第五题交付 435 行补丁后，原来的三个失败 nodeid 均消失；第七题交付 149 行补丁后，原来的 `test_session_accessed` 失败消失。二者都不再是“候选内测试通过，主根无补丁”。

独立核验八题最终测试的 cwd、Flask 实际导入文件均指向对应主 worktree，未从临时候选或父 AgentGo 仓库导入。Judge 执行前后被测输入 digest 不变，test_execution_ref 与 test_input_digest 匹配真实记录；所有 Completion/Delivery 已 committed。

### 4.4 Git 与终态判读

本轮记录 40 次包含 Git 的 Shell 调用，未再出现 Git packed object 长路径错误。独立本地回归验证 HEAD/index 不影响主根、下游基线来自父候选，以及 Git/文件视图一致。

八题都是 graph_terminal，settlement_verified=true，无 external_hard_kill，所有 Invocation/工具/reservation 均已结算。最后一题在 168 秒完成，因此正式批次没有自然触发 deadline 相撞；超时边界由真实二进制本地 SSE 用例与 Python 回归验证：新快照证明已交付且结算时正常结束，证据缺失或仍有在途事实时保持未完成。

## 5. 仍出现的错误及实际后果

正式批次有 44 次工具级拒绝/错误，不是 44 次任务失败：

| 类型 | 次数 | 含义与后果 |
|---|---:|---|
| Shell pipeline 退出码范围未声明 | 19 | 命令派发前拒绝；模型改写命令或明确范围后继续 |
| 结构化参数未知字段 | 13 | 多为业务结果字段放在顶层，另含拼写错误；没有写入终态 |
| 结果缺少声明字段 | 3 | 选择结构化提交后缺少 root_cause 等字段；模型继续纠正 |
| 文件路径读取失败 | 4 | 第二题把 Shell 的物理 .workspace-shell/.venv 路径传给逻辑文件接口；该调用不可复用，后续通过其它读取继续完成 |
| 文件精确替换不匹配 | 3 | old_str 已不匹配当前内容；拒绝错误写入后重新修改 |
| 错误种类的结果引用 | 2 | 第六题把 CandidateRef、OutcomeRef 放入 result_refs；拒绝后纠正为真正 ResultRef |

因此，不能声称模型从此不会误用工具。Shell 物理依赖路径转交文件工具、结构化结果的嵌套说明以及不同 Ref 的错误提示仍有可用性改善空间；这些是本轮新观察，不应再次隐藏在“全部绿色”之下。本次未添加兼容别名或放松校验掩盖错误。

另有 176 条实际 Shell 执行事实：154 条 success、22 条 failure。非零退出可来自故障复现或检查，不等于工具框架异常，更不等于最终 Judge 失败。

## 6. 验证证据与边界

- Windows 全量 Go 测试、vet、构建、70 项 Python 测试和双协议/动态 Team SSE 二进制用例通过。
- WSL Ubuntu 22.04、Go 1.25.0、CGO 开启：Graph/Workspace/Tools 的 race 检查通过；macOS amd64/arm64 交叉构建通过，未做 macOS 原生启动。
- 正式批次 Responses SSE 8/8 的 architecture_ok、model_contract_compatible、infrastructure_ok、execution_complete 均为 true；没有 provider HTTP 错误、未结算项、旧结果复用或遗留 AgentGo 进程。
- 测试根的 validation-identity.json、process-exit.json、runs/summary.json、comparison.json、live-analysis.json、error-analysis.json、final-verification.json 提供入口、逐题和复核证据。每题 runs 目录保留四阶段 pytest 报告、执行身份、result/judge、完整日志和 model.patch；worktrees/.agentgo 保留图、Context、候选、历史和 Trace。
- 旧批次及中止的诊断批次完整保留。本报告没有把旧 Trace 按新协议重新执行，也没有改写旧成绩。
- 这次通过证明该套 Flask-8 的修复及交付恢复，不等于所有工作负载、真实 Chat Completions、图片/文件输入或全部 UI 行为都完成验证。
