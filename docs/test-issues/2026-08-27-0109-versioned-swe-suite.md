# SWE-057：Flask 八题回归套件未随 runner 版本化

状态：**实现完成；Windows runner/suite/启动验证完成，实机八题业务结果待回填**。

## 现象

另一台 Windows 机器从 Git 获取 AgentGo 后运行 Python SWE runner，启动阶段报告
缺少 `tasks.csv`。该机器取得了受版本控制的 `harness.py`，却没有当前八题批次的
任务清单和 prompt。

## 根因与分层

此前的边界把 `tasks.csv`、prompts、worktree 和原始日志全部放在外部 testbed。
这个边界适合保护运行产物，却错误地把定义正式回归 cohort 的输入也排除在版本
控制之外，造成“runner 可复制、测试套件不可复制”。

这不是 AgentGo L1–L5 主链事故：文件缺失发生在任何模型调用、Context 装配、
Tool Lease、Loop 或 Graph 创建之前，属于外部 SWE 评测基础设施的封装/分发缺陷。
其中 prompt 在成功加载后是 L1 测试输入，但 prompt 未随仓库分发不是 L1 Runtime
契约漂移。

## 修复

- 新增受版本控制的 `scripts/swe_harness/suites/flask-8/`：
  - `tasks.csv` 使用八个完整 40 位 Flask commit SHA；
  - `prompts/` 保存八份任务正文；
  - `suite.json` 冻结 suite 身份、上游仓库、Python 版本和测试命令。
- runner 默认从仓库 suite 读取 task manifest 与 prompts；
  `SWE_TASKS_FILE` / `SWE_PROMPT_DIR` 仅作为自定义 suite 覆盖。
- testbed 使用平台用户数据目录动态拼装：Windows `%LOCALAPPDATA%`（回退
  `%USERPROFILE%`）、macOS 用户 Application Support、Linux `$XDG_DATA_HOME`
  （回退 `~/.local/share`）。Flask 源仓库默认位于 testbed 的 `upstream/flask`，
  不再写死开发者绝对路径。
- prompt 中的 `.venv/bin/python` 改为跨平台
  `uv run --no-sync python -m pytest -q`。runner 已先执行冻结的 `uv sync`，测试
  阶段不重新解析或修改依赖。
- API key、Flask 源仓库、worktree、reasoning、原始日志和运行结果继续留在外部
  testbed，不进入 Git。

## Windows 首次单题启动修复

Windows 首次运行 `automatic-options` 时，prepare 的目标红态与全量红态均符合
预期，但 AgentGo 在第 3 阶段读取生成配置后立即退出：`project_root` 与三个
`system_prompt_file` 使用了反斜杠，而 v4 配置路径红线只接受 forward slash。

根因是 Python runner 的 `yaml_template_value` 对所有值只做 YAML 反斜杠转义，
没有区分文件系统 `Path` 与普通字符串。修复后只对 `Path` 调用 `as_posix()` 并
兜底替换反斜杠；provider URL、model、token 等字符串保持原语义。

## 验证

- macOS 上传前 `python3 -m unittest scripts/swe_harness/harness_test.py`：43 项通过；
- Windows 路径修复后
  `py -3.13 -X utf8 -m unittest scripts/swe_harness/harness_test.py`：49 项通过；
- 默认配置能够解析仓库内八题，并核对每题完整 SHA、对应 prompt 和跨平台命令；
- Windows 本地完整 Flask 克隆为非 shallow 仓库，八个 manifest `fix_sha` 均能解析为
  commit；
- `automatic-options` 生成配置中的 `project_root` 与全部 `system_prompt_file` 均为
  `C:/...`；真实 `agentgo.exe config doctor` 返回错误 0、退出码 0；
- 同一配置使用 `-skip-startup-probe` 完成真实二进制启动，`/healthz` 返回 200；
  Ctrl+C 后空会话丢弃、锁文件消失且端口不再监听；
- `git diff --check`：通过；
- Windows 实机八题回归：待远端提交被另一台机器拉取后执行并回填。

本变更没有修改 AgentGo Context、Invocation、Loop 或 Graph 主链，因此不以本次
封装修复替代既有 8/8 基线，也不额外调用外部 provider。
