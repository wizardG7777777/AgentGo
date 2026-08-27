# Windows SWE Test Runner ReadOnly worktree 清理修复（2026-08-27）

## 状态

已修复。问题只影响 Windows 上重复使用同一考题 ID 时的 disposable worktree
清理，不影响已经完成的 AgentGo Graph、Judge 或仓库业务结果。

## 事故形状

第一次运行成功后，下一次 `setup_task` 会先调用 `safe_remove_worktree`。旧实现最终
直接执行 `shutil.rmtree(target)`；Python 在删除 `.git/objects` 下带 ReadOnly 属性
的 loose object 或 `.pack/.idx/.rev` 时返回 `PermissionError: [WinError 5]
Access is denied`，新一轮在 clone、网络和 AgentGo 子进程启动前失败。

实际失败目录没有 Git alternates，clone 也显式使用 `--no-local`，因此不是共享
hardlink 或上游仓库锁。成功重跑后同一目录再次出现 ReadOnly Git object，证明这是
可重复的 Windows 清理边界，而不是一次性权限漂移。

## 修复

`shutil.rmtree` 现在使用 Python 3.13 的 `onexc` 回调：

1. 只有 `os.name == "nt"` 且异常是 `PermissionError` 时进入补救；
2. 对失败路径设置 `S_IREAD | S_IWRITE`，随后只重试原失败操作一次；
3. 非 Windows、非权限错误以及重试失败全部原样抛出；
4. 保留既有 `testbed/worktrees/<task-id>` 精确父目录边界，不使用
   `ignore_errors`，不吞文件占用或真实 I/O 故障。

## 回归证据

- SWE Test Runner 契约测试：53 项通过；
- portable helper 测试覆盖 Windows 权限补救、非 Windows 与非权限错误拒绝；
- Windows 目录测试固定 ReadOnly `.git/objects/pack/*.pack`，同一路径连续清理两次；
- 真实 `%LOCALAPPDATA%/AgentGo/swe/worktrees/automatic-options` 首次含 11 个
  ReadOnly object，清理成功；重新准备后含 3 个 ReadOnly pack 文件，第二次清理
  同样成功；
- `go test ./...` 与 `git diff --check` 继续作为最终交付门。
