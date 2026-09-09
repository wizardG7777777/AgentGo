# 四类工具重建验证（2026-09-09）

验证对象是 4626bd3 之后、与本报告同一提交的工具重建代码。真实 SWE/Flask 题目与真实 provider 均未执行。以下本地 provider 使用 loopback HTTP、固定测试凭据及临时项目，不能用于声明真实模型成功率。

## 已执行结果

| 检查 | 结果 |
|---|---|
| Windows go test ./... -timeout 90s | 通过 |
| Windows go vet ./... | 通过 |
| Windows go build | 通过 |
| Python 离线 unittest（scripts/swe_test_runner/*_test.py） | 63 项通过；网络/pytest/任务事务使用 fake 或 mock |
| Windows Responses / Chat Completions 静态 Worker 二进制 | 两者通过；Graph success、revision=2、产物内容正确，各 23 次业务调用 |
| Windows Responses / Chat Completions 动态 Team 二进制 | 两者通过；无静态 Worker，Team 绑定未来图，各 25 次业务调用 |
| Ubuntu 22.04 WSL race | agent、contextruntime、mailbox、tools 四包通过 |
| Ubuntu 22.04 WSL 二进制 | 两协议均通过；Graph success、revision=2，各 23 次业务调用 |
| git diff --check / 当前规范文件链接检查 | 通过 |
| watchdog 生产代码差异 | 空；两处历史测试适配共享签名/策略引用 |

Linux 使用缓存中的 Go 1.25.0 官方归档，下载后按 go.dev 发布摘要验证，再以 CGO_ENABLED=1 执行 race。Windows 当前 CGO_ENABLED=0，因此 race 在 Linux 执行。

## 二进制覆盖的实际路径

1. 启动新运行时、配置与 L2 指令预检、SSE typed 工具能力探针。
2. apply_graph_change(create) 内部完成 Proposal Acceptance、路由与结构校验并提交；control_graph(start) 启动。
3. 运行中的 controller 读取图，提交删除根节点的非法修改并得到拒绝；随后合法更新未执行的 work，revision 从 1 到 2。
4. Worker 实际收到 LOCAL_WORK_V2；连续 read_file 八轮，不发生强制观察/决策或默认轮数停止。
5. send_message 只投递信息，空闲 observer 没有被创建任务；apply_change 生成 local-smoke.txt。
6. run_shell 在工作区执行命令，inspect_node 读取事实；Worker 提交真实文件产物。
7. Verifier 读取同一候选文件，引用上游 Evidence 提交 pass；Delivery promotion 后主根文件内容正确。
8. final-report 收口，所有当前 Run Task terminal；新版 runtime_audit 对 Context、完整模型输出、工具/Shell、使用量、图定义与 Outcome/ack 对账通过。
9. 两协议均验证中文文本分片、跨事件工具参数、完整结束。动态 Team 变体在初建前使用 graph_request_id，与随后 create 的 request_id 绑定相同图和 ready route。

## 在真实装配中发现并修复

- Proposal Acceptance 的旧 deadline 预留公式把未设时限的 Run 判为已过期：改为保留调用方取消，仅在显式 deadline 存在时约束调用。
- Graph 路由的验收工具闭集漏掉新检视工具：与 Lease 统一使用 agent.IsAcceptanceToolAllowed。
- 采集器误拒绝当前非 Graph TaskOutcome v1：按当前生产 DTO 和独立存储目录判断，避免把目录版本与业务 schema 混为一谈。
- 可选 Team 仍要求初建前知道 graph_id，且新 Scheduler 视图漏掉 Team 工具：以 graph_request_id 绑定未来 create 请求，图内 controller 只继承当前图，删除模型入口的旧 task-scoped 行为。
- L2 保留了不再需要的强制历史模式，并把去重路径键误写为 path/filepath：删除模式与 Observation 锚点路径，重复读取按真实 path 区分，Raw History 不变。
- Shell 超时丢失部分输出、默认退出码误写为 0，工具预留失败无结束回执：统一记录调用与进程事实，中断保留输出且退出码可缺席，拒绝仍记录未派发回执。
- Python 基线忽略“应不存在的测试文件路径被替换成目录”：补充路径类型检查，离线回归通过。

## 可复跑命令

```text
go test ./... -timeout 90s
go vet ./...
go build -o agentgo.exe .
py -3.13 -m unittest discover -s scripts/swe_test_runner -p '*_test.py'
py -3.13 scripts/local_fake_provider_smoke.py --binary ./agentgo.exe
py -3.13 scripts/local_fake_provider_smoke.py --binary ./agentgo.exe --protocol chat_completions
py -3.13 scripts/local_fake_provider_smoke.py --binary ./agentgo.exe --team
py -3.13 scripts/local_fake_provider_smoke.py --binary ./agentgo.exe --protocol chat_completions --team
```

Linux/macOS 使用 python3 与相应二进制；race 使用支持 CGO 的 Go 工具链。macOS 原生结果及真实 SWE/provider 媒体能力仍未执行，见 KNOWN_ISSUES。删除、迁移、重写、新增清单见工具重建删除对账。
