# UI Hub 改造问题与修复总账（归档）

> 归档日期：2026-07-19
> 范围：2026-07-18 多前端 UI Hub 改造前的风险摸排、修复与独立审计。
> 状态：已完成。当前开放限制见 [KNOWN_ISSUES.md](../activate/KNOWN_ISSUES.md)，不要把本文件中的已修复项目当成现存缺陷。

本文件保留已完成改造的可检索索引和验证结论；当前代码、测试及活动问题清单优先于这份历史记录。

## 已关闭项目索引

| 分类 | 已关闭风险 |
| --- | --- |
| A. UI 基础 | 多/零前端的通道消费者模型；无 ID/可能阻塞的审批协议；TokenStats 数据竞争；裸字符串输出与魔法分类 |
| B. Session 生命周期 | HistoryEmitter 旧句柄；PlanStore/TeamStore 旧目录；切换时跨 Session 污染；半切换状态；trace writer 与全局状态换绑；RunArchive 死代码；旧 session system.log |
| C. 落盘关键路径 | Plan 同步全量 fsync 和全局串行；同步 Reactor fsync；history/artifact 每事件 fsync 与 trace 全局锁 |
| D. 语义漂移 | map 序导致的随机任务；多套取消规则；重复的 profile/tools 解析；消费但未 emit 的 trace kind；状态词表与图标漂移 |
| E. 配置与运行卫生 | UTF-16 配置环境变量展开；明文 key 误报核实；空闲阈值死旋钮；异步 Reactor 关停；runner 主循环 recover；内部 Task 指针泄漏；单实例锁；text-only 结果的日志刮取恢复 |
| F. 收尾与低危审计 | Session 连续性语义；TUI 死代码；RunCLI 无效参数；active session TrimSpace；SessionConfig/LogWriter 死代码；失真注释；AllToolNames 镜像；短 ID；Spawn/Shutdown 竞态；CancelRegistry 锁；目录 fsync；Artifact replay；单消费者 BatchUpdate 信号；过时注释 |
| G. Windows 与测试 | Unix 临时路径；跨盘越界构造；`/dev/null` prompt；Bubble Tea 管道 EOF；反斜杠路径测试；验收 run 的 UUID 平局 flaky |

## 已落地的架构结果

- `internal/ui.Hub` 成为 `OutputCh`、`ApprovalCh`、`StatusCh` 的唯一消费者；TUI 与 Web Dashboard 都经它订阅、观测和控制，零前端时 Hub 也会排干生产者通道。
- Dashboard 具备认证、SSE、输入、取消、审批、模式、引导和 Session 控制；非 loopback 监听必须有独立 token。
- Session 切换以观测/持久化边界为语义：切换前保存旧 Session，相关日志和存储重新绑定；运行时任务、邮箱和 roster 不会被隐式重置。
- Plan、Team、trace、history、artifact 和结果快照的关停/恢复路径都有对应回归测试；计划内取消仍受 controller lease 保护。
- 已修复的 Windows 测试改用 `t.TempDir`、平台绝对路径和 forward-slash 配置路径；后续补丁又修复了 Bubble Tea 在非 TTY stdin 下绕开管道及静默吞掉 EOF 的生命周期问题。

## 验证记录

当时的完成门包括：

```text
go test -count=1 ./...
go vet ./...
```

并补充了 UI Hub、Dashboard、Session 切换、持久化恢复、Shell/TUI 控制与 Windows 路径语义的回归测试。两个 Windows Bubble Tea 生命周期测试因上游直接读取 `CONIN$` 而跳过；此项作为当前验证缺口保留在活动问题清单。

并发相关改动在这次归档时尚未执行 `-race`（Windows 环境无 C 编译器）。2026-07-19 的后续验证已使用 WSL GCC 与临时 Go 1.26.0 工具链运行全仓 race 测试：

```text
go test -race -count=1 ./...
```

该轮验证同时覆盖并修复了 POSIX 实例锁的 `os.ErrProcessDone` 判定、Shell 超时对子进程组的清理，以及非 TTY stdin EOF 退出；全仓未报告 data race。

## 独立审计结论

迁移前的独立审计确认上述修复均已进入生产路径，并补齐了 TeamStore 换绑失败回滚及 text-only 结果快照覆盖窗口两项接缝测试。该结论仅覆盖 2026-07-18 的改造快照；后续代码以当前测试与实现为准。
