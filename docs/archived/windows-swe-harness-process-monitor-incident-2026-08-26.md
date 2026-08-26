# Windows SWE Harness 进程监控事故修复（2026-08-26）

## 状态

已修复。修复范围是 `scripts/swe_harness/harness.py` 的 AgentGo 子进程生命周期
监控，以及同一 Windows 验证中暴露的批次文件时间边界竞态。

## 事故形状

- Harness 只把 AgentGo PID 传给 `monitor_run`，再用 `os.kill(pid, 0)` 判断存活；
- 当前 Windows 11 + Python 3.13 环境中，不存在 PID 会抛出未处理的
  `OSError: [WinError 87] The parameter is incorrect`；
- 对真实运行中的测试子进程执行该探测会令子进程异常退出，监控本身因而可能破坏
  被观察对象；
- 完整 Harness 自测还会偶发把刚写入的 result/judge 判为旧批次，原因是
  `time.time()` 与 NTFS 文件 mtime 的精度边界并非同一权威时钟。

## 修复

1. `monitor_run` 直接接收启动 AgentGo 时得到的 `subprocess.Popen`；
2. 每次轮询只调用 `process.poll()`：非 `None` 表示进程已退出，`None` 表示仍在运行；
3. 外部时限仍只形成 `external_hard_kill` 监控结论，实际终止统一留给
   `terminate_process`，监控查询不发送任何信号；
4. 批次通过 `record_batch_start` 写入 `.batch_start`，并以 marker 的实际
   `st_mtime` 作为 result/judge 新鲜度边界，保证比较来自同一文件系统时钟。

## 回归证据

Windows PowerShell 使用 Python Launcher 执行：

```powershell
py -3.13 -X utf8 -m unittest scripts\swe_harness\harness_test.py
```

结果：47 项通过。

真实子进程监控用例连续运行 10 次，失败 `0/10`；用例同时断言达到外部时限后，
被监控子进程仍然存活，随后才由显式清理函数终止。批次时间边界用例连续运行
25 次，失败 `0/25`。

## 尚未覆盖

本次没有运行真实 Flask SWE 单题或批次，因为当前 Windows 机器尚未恢复原始
`tasks.csv`、逐题任务描述和带目标 fix commit 的 Flask benchmark 仓库。Provider
探针通过不替代这组业务端到端证据。
