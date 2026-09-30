# AgentGo SWE Test Runner

本目录是外部 SWE 测试程序的唯一入口，负责多套件考题准备、AgentGo 进程管理、pytest/Go/原生框架判题、批次汇总和资源收尾。它不属于 L3 Harness Engineering。工具重建的适配计划与阶段验证约束见[工具契约第 12–13 章](../../docs/design/tool-taxonomy-and-contracts.md)。逐次运行结果保存在本地 testbed，不纳入 Git。

Ubuntu 24.04 Flask 系列镜像见 [SWE_LinuxContainers](../../SWE_LinuxContainers/README.md)：镜像内置脚本/题库及 pip 预装环境，
启动时必须显式提供匹配 CPU 架构的 AgentGo Linux 二进制，目标容器不构建、不执行 pip install；
个人明文 `config.json` 只做文件存在性门禁。默认导出报告/行为日志后销毁本批容器与数据卷；
`--persist` 保留容器及包含完整配置的内部现场。
`SWE_PREBUILT_ENVS` 指定按题目预制的 Python 环境，`SWE_PROBE_ARCHIVE` 启用 Python 前置探针原文归档；
未设置时保留原有环境准备和探针行为。容器配置门禁不替代本 Runner 的四变量/协议契约。

## 启动条件

执行类入口 probe/task/batch/verify-candidates 先一次性校验以下四项非空环境变量；缺项时在网络、文件写入和子进程启动之前退出，诊断只列变量名。离线 storage/cleanup/archive 命令不要求模型变量，也不调用 provider：

- `SWE_API_KEY`：provider 密钥，仅从进程环境读取。
- `SWE_BASE_URL`：OpenAI-compatible API 基础地址。
- `SWE_FAST_MODEL`：快速能力档位。
- `SWE_FLAG_SHIP_MODEL`：旗舰能力档位。

两个模型可以相同，通用能力探针按实际模型去重。旧 `SWE_MODEL`、`SWE_BASE_MODEL`、`SWE_WORKER_MODEL` 不作为 fallback。角色模型由 [setting.swe-flask.yaml](../../setting.swe-flask.yaml) 决定：Scheduler/Verifier 使用快速档，Explorer/Worker 使用旗舰档。已删除独立 Observation 模型配置及多版本 Observation 探针。

Flask 原生启动需要 Git、uv、Python 3.13、已构建的 AgentGo，以及包含目标 fix commit 的完整 Flask Git 仓库。
容器路径使用镜像预制的 Python/pip 环境，跳过原生 uv sync 准备步骤，容器不安装 uv。
公开入口自行将 stdout/stderr 配置为 UTF-8；Windows 不依赖调用者设置代码页或 PYTHONUTF8。

可选变量：

| 变量 | 默认值或含义 |
|---|---|
| SWE_PROTOCOL | responses；也可明确选择 chat_completions，两者均只使用 SSE |
| SWE_AGENTGO_ROOT | 从 runner.py 推导仓库根 |
| SWE_AGENTGO_BIN | 仓库根的 agentgo.exe（Windows）或 agentgo（macOS/Linux） |
| SWE_TESTBED | 当前用户的平台数据目录下 AgentGo/swe 或 agentgo/swe |
| SWE_SUITE_DIR | scripts/swe_test_runner/suites/flask-8 |
| SWE_TASKS_FILE | suite 下的 tasks.csv |
| SWE_PROMPT_DIR | suite 下的 prompts |
| SWE_UPSTREAM_REPO | 可选的上游 Git 仓库路径；默认 testbed/upstream/项目名 |
| SWE_FLASK_REPO | Flask 专用的既有路径变量；通用变量优先 |

testbed 默认目录：Windows `%LOCALAPPDATA%/AgentGo/swe`（缺失时使用 USERPROFILE）；macOS `~/Library/Application Support/AgentGo/swe`；Linux `$XDG_DATA_HOME/agentgo/swe`（缺失时使用 `~/.local/share/agentgo/swe`）。不硬编码用户目录。正式八题 manifest、题目 prompt 与 suite.json 随仓库提供；testbed 只保存源仓库、临时 worktree 和运行产物。

## 完整事务命令

在仓库根运行，实际成绩以具体批次报告和归档为准。

```powershell
go build -o .\agentgo.exe .
py -3.13 scripts/swe_test_runner/runner.py probe
py -3.13 scripts/swe_test_runner/runner.py task automatic-options --timeout 1200
py -3.13 scripts/swe_test_runner/runner.py batch --timeout 1200
py -3.13 scripts/swe_test_runner/runner.py verify-candidates
```

macOS/Linux 使用 `go build -o ./agentgo .` 和 `python3` 替换对应命令。四项 provider 环境变量必须已在当前进程中设置。密钥不要写入命令行参数或版本控制的 YAML。

- `probe`：逐模型验证所选协议的 SSE 完整结束、typed function call、非空且唯一的 call_id 和正确 nonce。文本声称调用了工具不能通过；不再启动 `agentgo probe observation`。
- `task`：通用探针 → 准备红态与基线 → 启动 AgentGo → 收集新运行事实 → 独立 Judge → 汇总。
- `batch`：按 tasks.csv 串行执行完整单题事务，每题后和 finally 原子更新本批次汇总。
- `verify-candidates`：干净基线 → golden tests 红态 → golden source fix 绿态，验证题目本身有效。golden source fix 不交给修复 Agent。

`--timeout` 只限定外部评测进程运行时间；不换算成 Agent 的轮数、阶段预留、token/cost 上限或强制 Recovery。旧 240/480 秒下限已删除。HTTP 与单条 Shell 的传输/进程超时仍是各自配置的执行限制。

## Gin 原生套件（PowerShell / zsh）

同一入口增加 `--suite gin-1`，可放在子命令前或后；省略时仍使用 Flask-8。
Gin 的清单为 `suites/gin-1/`，目前包含 `context-copy-state` 一题：修复 Context.Copy 的 Errors/Accepted 状态复制。
套件为 `agentgo.swe-suite/v2`，固定上游提交、Go 模块和 `go1.25.0`；原 Flask-8 保持 suite/v1 与 pytest 判题。

需要 Git、Python 3.13、Go 和当前平台的 AgentGo 二进制。启动前仍校验同一组四个 SWE 环境变量，
然后执行同一能力探针、Run 创建、Graph/Delivery 审计和结果汇总。Gin 的上游仓库缺失时自动克隆，
依赖由 Go 准备；无需 Flask、uv 或容器。测试缓存通过 `-count=1` 禁用，依赖文件只读，`GOWORK=off`。

PowerShell：

```powershell
go build -o ./agentgo.exe .
py -3.13 scripts/swe_test_runner/runner.py verify-candidates --suite gin-1
py -3.13 scripts/swe_test_runner/runner.py task context-copy-state --suite gin-1 --timeout 1200
# 同一套件的完整批次入口
py -3.13 scripts/swe_test_runner/runner.py batch --suite gin-1 --timeout 1200
```

zsh：

```zsh
go build -o ./agentgo .
python3.13 scripts/swe_test_runner/runner.py verify-candidates --suite gin-1
python3.13 scripts/swe_test_runner/runner.py task context-copy-state --suite gin-1 --timeout 1200
```

未设置 SWE_TESTBED 时，Gin 使用平台默认 SWE 数据目录下的 `gin-1/` 子目录；
设置后直接使用指定路径。正式实验建议使用独立目录，避免覆盖同题旧产物。
SWE_UPSTREAM_REPO 可复用已经下载的完整 Gin 仓库；它是准备阶段的输入，不向修复 Agent 提供官方源码补丁。
角色模型、工具权限和 880000-token 窗口继续由 setting.swe-flask.yaml 提供，Gin 只替换三个角色的 Prompt 路径。

### Go 正式判题与平台基线

`native_go.py` 负责原生 Go 测试，不新增产品层工具或 Graph 节点类型：

1. 完整执行干净基线，记录原有失败及每个顶层测试的结果。
2. 只应用官方测试补丁，确认目标红态；源码修复不交给 Agent。
3. AgentGo 完成图级交付后，运行全部目标测试和 `go test ./...`。
4. 目标测试必须全部通过且无跳过；所有原先通过的测试仍须通过；回归测试集合必须与准备阶段一致。
   不允许新增失败、漏跑、跳过原先通过的测试，或修改测试/依赖配置。

Gin 判定为 `target_failures_fixed_and_full_baseline_preserved`。`full_suite_green` 单独说明是否全套零失败。
当前固定 Gin 基线在 Windows 存在 `TestFileDescriptor`、`TestSaveUploadedFileWithPermission` 两个已有失败，
会保留在 `preexisting_failures`，不会删除或跳过对应测试；它们不是本题修复目标。
本次 Linux zsh 基线没有这些失败。逐次平台结果保存在本地 testbed，不纳入 Git。

每阶段保留 `.collection.jsonl`、`.go-test.jsonl`、`.go-test.log`、`.go-test.json` 和 `.go-test.execution.json`。
Go 报告使用 `agentgo.go-test-report/v1` / `go-top-level-tests/v1`：顶层测试只计数一次，子测试失败另外保存，
panic 后未执行的测试显式列入 not_run；不能把编译失败、截断流或零测试当成通过。
执行身份记录实际模块目录、精确 Go 版本、依赖校验、测试 argv 与源码/测试/资源前后摘要。
源码身份覆盖 Git 跟踪/未忽略文件，以及即使被忽略的 Go/原生编译输入和测试资源；Gin 生成的 ignored `test` 文件属于测试输出。
正式 Go 子进程不继承 SWE/AGENTGO 凭据，真实模型凭据仅供 AgentGo 使用。

## Core-96 原生套件

题库、原生测试驱动与受管资源生命周期已接入 [统一 Runner](runner.py)，设计约束见[资源生命周期方案](../../docs/design/swe-native-storage-lifecycle-plan.md)。截至 2026-09-24，Core-96 的 192 项双平台评测均已结算，177 项修复成功、15 项失败；结果来自多个冻结批次和不同二进制身份，不代表单一版本从零统一重跑或全数通过。逐题结果与归档留在本地 testbed，不纳入 Git。

### 原生资源入口

使用独立的 Python 3.13 Runner 环境，先安装 `requirements-runner.txt`；原生管理最低支持 Python 3.12。被测项目的依赖仍安装在各自环境中。

原生入口默认创建 `storage-root.json`、独立 `batches/<id>/attempts/`、`cache/`、`archives/` 和 `cleanup/`。请使用新的 SWE_TESTBED，不把历史未登记目录交给新清理器自动接管。Core-96 必须通过 `--storage-policy` 或 SWE_STORAGE_POLICY 明确提供容量策略；`storage-policy.example.json` 是需经峰值校准的初始策略。

实际工作树使用短路径 `w/<attempt-prefix>`，阶段临时目录使用 `t/<attempt-prefix>/<phase-number>`；完整逻辑索引和报告仍在批次目录中。请保持 SWE_TESTBED 简短，默认 scratch 前缀上限为 80 bytes，以兼容原生工具和 Unix socket。`SWE_UPSTREAM_ROOT` 可指向已有的上游镜像父目录，其下按项目名查找仓库，仅作为借用输入。

受管命令另分配 `t/<attempt-prefix>/x/<scope-id>`，保存 scratch 身份与权限回执。POSIX Cargo 测试程序通过普通用户执行钩子获得独立 scratch；原始 stdin/stdout/stderr 仍由调用方持有。进程树确认退出后才恢复生成临时目录的必要收尾权限，原模式留证，外部链接不跟随；Windows 继续使用既有 ReadOnly 清理规则。短暂权限拒绝只记为不完整盘点，仍受新鲜度和最终完整盘点约束，不能忽略未知占用或在测试运行中修改权限。逐次验证留在本地 testbed。

```text
python scripts/swe_test_runner/runner.py verify-candidates --suite cobra-8 --task-id cobra-746ef0715872 --storage-policy scripts/swe_test_runner/storage-policy.example.json
python scripts/swe_test_runner/runner.py storage report --testbed <testbed>
python scripts/swe_test_runner/runner.py archive verify --manifest <manifest.json>
python scripts/swe_test_runner/runner.py archive seal --attempt <attempt-directory>
python scripts/swe_test_runner/runner.py rejudge --attempt <archived-model-attempt-directory>
python scripts/swe_test_runner/runner.py cleanup plan --attempt <attempt-directory>
python scripts/swe_test_runner/runner.py cleanup apply --plan <plan.json>
```

`archive seal` 只在有可核对的退出事实时封存中断现场，不恢复模型执行；失败现场默认保留，核对归档后可在生成计划时显式指定 `--release-retained`。清理成功、归档成功、测试通过是三个不同结论。

新资源归档使用 `agentgo.swe-artifact-manifest/v2`。非 UTF-8 文件名保留在清单中，并映射到 ZIP 可编码的成员名；校验映射、路径、内容与成员集合后才能清理。`read_member` 按原逻辑名称读取，无需解压或跟随链接。新版工具继续只读校验旧 v1 归档，历史归档不重写；新格式须使用支持 v2 的工具读取。

`rejudge` 用于判题器修复后的原补丁复判，不读取模型配置或调用 provider。仅接受已停止、已结算、归档校验通过且没有测试篡改的原生模型尝试；要求原平台和题目定义一致，并重新核对资格测试清单。在新的受管尝试中应用原始补丁，核对修改后源码与归档摘要，再运行正式判题。旧报告保持原样，新增报告记录原尝试、归档和补丁摘要及 `new_model_calls=0`；不重放 Graph/Session，不把复判重复计为一次模型评测。依赖准备仍可能访问包仓库。

Vitest 3/4 使用官方 Node API 加载上游配置和测试规格，在执行前按声明文件/目录的真实路径精确选择；保存 `vitest-selection.json`。CLI 过滤器只作为发现提示，不能作为边界依据，因为绝对路径仍可匹配相同后缀，且多项目未必继承 CLI exclude。归一化报告发现框架副本或声明范围外的测试时拒绝整个报告；不在事后删掉失败条目。合法用户测试中的嵌套 `.agentgo` 目录保持可见。

旧判题在保存补丁前发生基础设施错误时，`rejudge` 可从已校验归档重建交付源码；必须有原失败回执和已结算执行证据，归档中的测试/配置摘要与原冻结基线、新尝试基线完全一致。新报告记录源码树摘要及原 judge 缺失的事实，不补写旧报告。

原生判题在执行测试前保存 `model.patch`，其范围为 Git 可见的生产文件。Git 忽略的临时源码不强制暂存，另记 `ignored_source_artifacts.json`，继续保留在测试输入身份和归档中。复判原补丁时也恢复这些已记录输入，并核对忽略/未跟踪状态、受保护路径和归档摘要；补丁文件不冒称包含整个交付工作树。

Cargo 复判在依赖安装前恢复归档中的原 Cargo.lock，要求其摘要与原测试基线一致，并使用 `cargo fetch --locked`。原锁进入环境身份和独立资格缓存；重新生成的不同锁不能用来替代原依赖，也不能放宽受保护输入检查。

Cargo 测试按 build artifact 的 package_id 与 metadata 清单确定所属 package，查询及执行测试均使用该包根目录，保存 `cargo-execution.json`；不能统一从 workspace 根运行。复判的目标、收集和测试身份必须一致；修正判题环境后通过集合可增加，但不能减少旧通过项或引入未知测试，新增通过项单独记录。

`.tsbuildinfo` 是 TypeScript 构建缓存；普通目录中的缓存不按 `tsconfig` 或 `.test.` 文件名前缀误判为测试配置。真实测试目录和题目明确声明的文件继续受保护。旧误判的复判仅接受原基线没有、未跟踪且被 Git 忽略的新增缓存，逐项验证原归档文件并在来源记录中列明；真实测试或配置变化仍拒绝。

Vitest 的原始基线允许测试补丁尚未新增的文件不存在：仅在 `clean` / `clean-baseline` 阶段，通过固定 base/fix 的 Git 新增文件记录与 `test_files` 交集确认，并保存证明。已有声明测试照常执行；完全由新增文件组成的范围显式记录空基线（0 测试、0 通过、`tests_executed=false`）。补丁应用后的红绿验证和正式判题不允许缺失，路径拼写错误、越界和测试被删除仍拒绝。

`archive_max_bytes` 限制整个 testbed 的历史归档总量（包含失败的 `.partial` 文件），默认 32 GiB。每题准入先预留 `archive_reserve_bytes`，ZIP 实际写入和元数据也受预算约束。预算不足时停止新题或保留尚未封存的现场，报告 `archive_budget_exceeded`；不会为了继续测试自动删除历史证据。归档只保留在本地，长期保留或转存后删除需要明确的证据处置记录。该限制与缓存、在途现场预算分别计算，不能把归档预留空间当成历史容量上限。

目录盘点使用独立、只读的 Python 进程，避免 Runner 的 CPU 密集解析阻塞文件属性查询。进程身份和退出事实写入 attempt.json，超时会终止并等待，活跃盘点进程会阻止归档/清理；这仍是采样保护，不是文件系统硬配额。受管 Go 命令通过 GOFLAGS=-p=N 使用 max_build_jobs。结束并校验归档后，生成的 setting.yaml/setting.yaml.tmp 会删除，包括保留失败现场的情形；环境中的原始凭据不受此操作影响。

2026-09-18 起临时采用 `max_scan_seconds=450`、`max_inventory_age_seconds=600`；策略加载拒绝样本有效期不覆盖扫描预算及扫描间隔的组合。磁盘余量检查仍约每 2 秒进行，目录容量上限没有提高。`max_monitor_stop_seconds=30` 单独限制命令监控线程收尾，扫描线程仍按其扫描截止时间有界退出。

运行中使用 `logical-with-cache-refinement` 盘点：读取所有区域的逻辑占用，未计算的 `unique_file_bytes` 和 `allocated_bytes` 为 null；逻辑占用是唯一文件占用的上界。缓存逻辑量超过阈值时才精确去重，避免硬链接导致误判。收尾使用 `exact` 完整计量，报告不得把运行中的未知值当作 0 或精确值。目录枚举先保存当前目录的条目并关闭句柄，再处理元数据；不会一次保存整棵目录树，也不排除 node_modules 或工作副本。每个尝试的 `reports/inventory-samples.jsonl` 保留各次结果与耗时，`attempt.json` 仍提供最新状态。

运行期 worker 的所有权/布局/策略来自父进程在 Attempt 锁内复制的 `agentgo.swe-observation-snapshot/v1` 快照，结果注明 `state_source=supervisor_snapshot`；快照不包含完整运行状态或凭据。worker 仍检查根标记、实际目录身份、路径和策略，拒绝缺失快照的运行请求。模块内直接离线 `collect` 可继续读取停止尝试的磁盘元数据，不能把该离线方式当作运行期 worker 的替代。

Windows 的 scratch 恢复读取与 Attempt 持久化共用锁，避免文件原子替换期间的读取竞争。`command.json` 分别保存 `process_cleanup_error`、`scratch_recovery_error` 和 `scratch_recovered`，含操作名、原异常类型、errno/winerror 和有限原因链；Job 查询/终止失败保留其原生错误。已确认进程树退出后，scratch 失败不会抹去 `quiescent=true`，但命令仍失败、`output_complete=false`，不会被算作成功；真正进程未知仍保持 false 并停止。

盘点的 Windows 5/32/33 错误会记录操作名、errno/winerror 和路径复核结果，然后按 25/50/100/200 ms 间隔最多追加 4 次只读操作；整次采样所有区段共用 1 秒等待额度，扫描截止时间仍有效。等待前后复核从受管根到叶子的路径；已经消失的条目跳过，叶子链接只计链接，父目录变成链接则停止。目录枚举失败时关闭句柄、丢弃局部结果后重新枚举，避免重复计量；文件元数据重试使用新 lstat，避免复用旧 DirEntry 缓存。持续错误与未知错误保持不完整/停止，Windows scratch 不沿用 POSIX 权限测试的宽限。`read_diagnostics` 保留计数及最多 4 个故障样例，相对路径超过 512 字符时明确截断；致命错误另有顶层详情。此机制不修改权限、不调用模型或重放业务命令，后续成功盘点也不会抹去已经触发的停止原因。

已登记的可再生环境在测试阶段开始及被测进程启动前写入 `.swe-rebuildable-directory.json`。归档只对相对路径后缀和标记摘要均匹配的副本继承排除，并在 manifest 的 `omitted_copies` 中记录来源资源及省略路径。未标记、标记变更或改名的目录仍保留，符号链接不跟随；不会按 AgentGo 的内部目录名裁剪。逐次归档清单保存在本地 testbed。

### Windows Node 依赖目录

Vitest Driver 在 Windows 安装依赖后，把自有 node_modules 内的 NTFS 目录联接转换为相对目录符号链接。先不跟随链接地盘点全部目标并核对工作树边界，再验证当前用户的符号链接权限；转换只更换联接条目，不展开或复制依赖目录，循环依赖也保持为链接。相对链接使工作副本中的 workspace 包解析到该副本。

权限不足、目标越界、未知重解析点或身份变化会停止准备。不会提权测试进程、启用系统开发者模式或递归复制整个依赖图。`.swe-build/dependency-links.json` 记录转换数量和映射摘要，并纳入测试环境身份。Linux/macOS 保持原依赖布局。

Windows 工作区复制保留源符号链接的文件/目录类型；该实现由平台测试覆盖。单项验收记录留在本地 testbed，不外推为其他 Node 仓库或全部题目的验证结果。

### Windows 自动网络授权

原生执行子命令可加 `--windows-firewall`，在开始时请求一次 UAC。管理员辅助进程只添加/删除本批测试的防火墙规则，测试与 AgentGo 仍以原来的普通用户权限执行。辅助代码经摘要校验复制到 Program Files 下受保护的目录，以当前 Runner 的 PID 身份为生命周期；不会修改全局防火墙开关、默认入站动作或通知设置。

启动截止时间覆盖 UAC 等待；取消、拒绝、启动失败或超时均停止，不自动重试。每个 Windows 用户同一时刻只允许一个助手或待授权启动。命名管道读写也有独立截止时间，辅助进程不继承模型凭据。新增可见防火墙窗口会触发取消，防止未覆盖的执行路径继续堆积提示。

Go 通过统一环境中的 `-exec` 钩子，在临时测试 EXE 启动前申请精确路径规则，覆盖 Runner 判题以及继承该环境的 AgentGo `go test`。Cargo 使用 target runner；直接执行的已登记 EXE 和 CTest 已知测试程序走执行前登记。规则的远端地址限定为 `127.0.0.1`，全部网络配置文件适用；Windows 拒绝将 IPv6 回环地址 `::1` 写入该规则，因此 IPv6 本机通信由实机检查单独验证，不扩大地址范围。任意自行绕过这些入口的子程序不属于已验证覆盖范围。

整个多套件调度可先调用内部 `firewall_windows.start()`，向 Windows 子 Runner 传递 `SWE_WINDOWS_FIREWALL_SESSION` 复用同一个助手，从而只确认一次 UAC。Linux/macOS 不启用该分支。每题在确认进程退出后回收规则，`reports/firewall.json` 保存回执；授权失败、助手失联或明确阻止规则冲突按基础设施错误处理。不要把整个 Runner 以管理员身份运行。

受管命令启动前申请所需程序的 lease，记录到 `command.json/firewall_permissions`；确认命令及子进程退出后立即归还，整题 `release_run` 继续兜底。一个命令中的重复程序只申请一次；Python venv 启动器及基础解释器全部纳入该命令，CTest 选中的子程序归属于对应 CTest 命令。并发命令仍各持有独立 lease，最后一个使用者退出才删除规则。启动失败会回收已申请部分；进程状态未知或释放失败继续停止并留证，不假称清理成功。助手的 256 个活动 lease 上限保持不变；顺序执行次数不再累积为活动授权数量。

拥有助手的入口退出时，先回收规则、保存回执，再确认移除受保护安装目录；借用助手的子 Runner 只结束自己的观察与租约。暂存文件及控制记录位于受管 `batches/<id>/firewall/`。崩溃后可能保留受保护的恢复记录；下一次同用户启动只处理已核实原主人退出的自有会话，结果写入新批次，不触碰用户手工规则。

`firewall-check` 是不调用模型的 Windows 验证入口。它连续运行三个不同临时路径的 `gin.test.exe`，检查执行前已有精确规则、测试进程非管理员、本机通信、没有新可见防火墙窗口、规则回收及全局策略保持原样。执行类入口使用 `--windows-firewall` 时也先运行这项检查，通过后才进入 provider 探针和 SWE。

`firewall-check` 是 Go 网络验证夹具，不计作 Gin 模型修复成绩，也不代表 Cargo/CTest 的实机验收。窗口观察范围为新出现的可见顶层窗口；规则回执保存在本地 testbed。

```powershell
python scripts/swe_test_runner/runner.py firewall-check --testbed <short-testbed-path>
python scripts/swe_test_runner/runner.py batch --suite gin-8 --windows-firewall --storage-policy scripts/swe_test_runner/storage-policy.example.json
```

原始命令输出保存在每条命令的独立目录，阶段报告通过引用关联。成功归档清理后，快速报告保留在 reports，原始命令、源码和 scratch 证据可在归档中读取；`storage.json` 和清理回执记录峰值、保留理由与实际回收结果。原始日志可能含敏感内容，不提交 Git。

容器设置 SWE_RESOURCE_OWNER=container，继续由现有容器外层管理独立卷及导出；原生与容器不能同时认领同一批资源。

`core-96` 组包含 gin-8、cobra-8、bbolt-8、gorm-8、sympy-8、zod-8、vue-8、
excalidraw-8、tokio-8、ripgrep-8、fmt-8、pytest-8，每个仓库恰好 8 个独立修复提交。
当前题库包括 12 个仓库、每仓库 8 题。正式模型成绩与资格验证分别记录；Core-96 的最近一轮 192 项评测已经结算，不能把 177 项成功误报为全数通过。

```powershell
# 一个仓库的八题资格验证
py -3.13 scripts/swe_test_runner/runner.py verify-candidates --suite cobra-8
# 调试某题的完整红绿资格事务
py -3.13 scripts/swe_test_runner/runner.py verify-candidates --suite cobra-8 --task-id cobra-746ef0715872
# 全部 12 个仓库的资格验证与正式模型批次，仍使用同一入口
py -3.13 scripts/swe_test_runner/runner.py verify-candidates --suite core-96
py -3.13 scripts/swe_test_runner/runner.py batch --suite core-96
```

zsh 使用 `python3.13` 替换 `py -3.13`。环境必须提供各套件所需的 Go、Python、Node/package manager、Rust、CMake/Ninja/C++ 编译器。
`SWE_NATIVE_TOOLS` 可指定外部原生工具/依赖缓存目录；该目录不进入源码或 Git 提交。
普通单题使用 `task <task_id> --suite <repo>-8`。

suite/v3 和 tasks.json 显式记录驱动、基线/修复提交、测试与源码补丁边界、工具环境、目标与回归范围。
Go、pytest、Vitest、Cargo/libtest、CTest 的原始结果分别保留，公共报告为 `agentgo.native-test-report/v1`。
真实模型的 Run/Graph/Delivery 审计与 `result.json` / `judge.json` 继续共用原入口。

回归范围逐题声明：Go 通常运行完整模块；GORM 覆盖根模块和 tests 模块的 SQLite 测试；
SymPy/pytest 与前端库采用明确的相关测试模块；Cargo 运行声明包；fmt 运行 CTest 项目。
bbolt 使用官方普通单元测试包的 short 模式，故障注入、断电健壮性和上游 short 跳过的压力测试单列为未覆盖。
以上范围不能被称为所有项目的全仓全模式测试。

资格验证允许上游测试补丁合法重命名或参数化旧测试，并记录身份变化。
模型判题必须保留官方修复后参考集合中的全部通过项、修复所有目标失败，测试集合与参考保持一致，不能新增失败或以跳过代替通过。
同名 Vitest 测试按文件中的声明顺序分别编号；Go 父测试计数与子测试结果分别保存。
资格报告绑定题目定义、平台和加载时的判题器代码摘要；重跑产生独立 attempt 目录，原始证据不会被当作新运行覆盖使用。

原生题目可声明 `required_regression_tests`（完整报告测试 ID）：除未应用测试补丁的 clean 阶段外，必须收集并执行这些回归，红态必须实际失败；缺席、跳过或只有无关失败均拒绝。`test_adaptations` 是显式平台测试夹具适配：只允许声明的 test_files，固定平台、上游/改写后 SHA-256 和唯一精确替换，拒绝链接与源码路径。适配在应用上游测试补丁后、冻结模型测试输入前执行；摘要进入题目身份和执行报告，正式 Judge 继续保护这些测试。不是运行后过滤成绩，也不是放宽红绿要求。ripgrep `241b87b33771` 的 Windows 适配使用相对 excludesFile 路径执行原断言，Linux 保留原测试字节。

测试适配的输入摘要绑定 Git 的 LF 正文；Windows 检出的 CRLF 仅在边界归一化，其它字节变化仍拒绝。回执另存实际读取字节的摘要及行尾转换类型，适配后输出固定 LF；回执进入受管 reports 归档，不能用行尾归一化掩盖测试内容变化。

ripgrep `4df129812769` 的原测试依赖 Unix 尾点目录。Windows 使用同一 `ignore::overrides` 公开匹配接口，分别断言 `!asdf/` 与 `!asdf./` 对普通/尾点目录路径的四种匹配结果，保留区分尾点名称的缺陷目标，不通过实际 Win32 文件创建改变输入语义。上游源码修复和 Linux 文件系统回归保持不变，两端都须验证指定测试红态失败、绿态通过。

题目替换必须保持 base/fix、目标回归和题面身份一致；旧题结果只留作历史，不能抵算新题。当前题库清单是正式来源，逐题资格和模型运行结果保存在本地 testbed。

模型工作视图排除 node_modules 与 .swe-build 的候选源码计数，仍保留 Shell 使用依赖的能力；
绑定旧源码路径的 .swe-build 构建缓存不复制到新工作视图。Zod/ripgrep 声明的仓库内部文档链接在准备阶段实体化，
该环境处理发生在基线测试之前，不包含生产修复或测试修改。

详细实施范围与未完成项见 [Core-96 记录](../../docs/design/swe-core96-native-expansion.md)。

## 输入与工具边界

Python 使用 `agentgo.run-contract/v3` 创建 Run 身份和 `swe/v4` 记账标签，不注入 CheckContract、check_id、exact_command 或阶段 reserve。Agent 可以用 run_shell 自行运行 pytest 调查；AgentGo 只保存通用命令执行事实。正式“测了什么、测的是哪份代码、测试是否通过”由 Python 判定。

Worker 使用 run_shell/read_file/apply_change 及结果、检视、通信工具；Explorer 使用读取、检视与 Shell 调查，不承担文件修改；Verifier 读取上游结果和证据并提交结论，无 Shell 或文件写入权限。未配置独立 Web 工具不能证明无网络，Shell 的网络能力必须由实际执行环境约束。

基线失败日志仍被归一、去 ANSI 并裁剪为最多 6000 字符的 `<swe-baseline-failure authority="swe-test-runner">`，只加入本次用户输入，不改写题目 prompt，不形成 Observation 或决策工具前置条件。题目红态不是修改 tests/ 的授权。

## 正式判题记录

原生源码补丁导出将明确路径清单用 NUL 分隔，经 stdin 传给 `git --literal-pathspecs add --pathspec-from-file=- --pathspec-file-nul`。这避免 Windows 命令行长度上限，并将空格、方括号等按实际文件名处理。独立 index 重置后仅暂存选定生产文件，额外核对 staged 集合，再导出二进制兼容补丁；不会顺便暂存测试或被忽略文件。

CMake 外部构建目录中的已知产物按 `native_build_artifacts.py` 分类，要求 CMakeCache 的源/构建关系一致，且没有 Git 已跟踪或原始输入落在该目录。已知构建文件、编译器识别源码和构建测试目录的运行产物保留在输入摘要及归档，但不当作测试修改或交付源码；产物不必全部被 Git ignore。未识别源码、声明测试和真实测试修改仍拒绝；不能仅凭目录名 `build*` 跳过检查。复判旧误报时另核对原测试摘要、原归档和新基线的跟踪状态；TypeScript 缓存的既有例外仍要求 ignore 证明。重分类证据单独保存。

run_pytest 使用实际虚拟环境 Python 和 argv 测试路径，保留 pytest 原文、JUnit 与 `agentgo.pytest-phase-report/v2` sidecar。插件按 nodeid 和阶段记录失败，允许 call failure 与 teardown error 重叠；不从 JUnit 总数相减猜 passed。

每次正式 pytest 另保存 `agentgo.swe-test-execution/v1`：

- 题目、Run（准备/候选自检阶段为空）、阶段、测试执行身份；
- Git 基线提交、argv、cwd、Python 路径、时间与退出状态；
- 测试前后源码/测试/资源/配置的文件集合与实际字节摘要，包括未提交增删；
- 实际 Flask 导入路径、Python/依赖环境、收集 nodeid；
- 完成、被中断或无法可靠归属的状态。

正式测试要求 Flask 从被测 `src/flask` 导入；测试期间输入发生变化时拒绝归属结果。测试保护基线 v2 覆盖测试集合及 pytest 配置的增删/修改。Judge 对比具体失败项与阶段；失败总数相同不代表没有新增破坏。无测试收集、计数冲突、错误 schema 和缺失报告不能通过。

## 运行事实与结果

`runtime_audit.py` 读取新版运行记录，不根据旧轮次阈值或固定工具序列判分。工具请求、Registry 派发、Shell 实际执行、模型输出和 Python pytest 是不同事实。非零 Shell 退出本身不等于架构错误；取消、超时和启动失败不伪造 exit=0。

当前目录位于 `.agentgo/state`：`context-snapshots-v3`、`task-outcomes-v4`、`run-usage-v2`、`loop-facts-v3`、`graphs-v8`、`deliveries-v3`、`model-probes-v3`。图 authoring/completion 共用 graphs-v8 日志。Session 模型完整输出仍在各会话 turns.jsonl，候选在 `.agentgo/candidates-v1`。目录版本与内部记录 schema 分别校验，不向旧目录 fallback。

Context 本体没有 RunID，通过本 Run 的 InvocationID 关联。工具按 Run/Task/Attempt/Invocation/CallID 对账；重复相同记录不增加计数，冲突、缺失及损坏记录报告为证据问题。未知值不当作有效的零值，截断日志保留可读前缀且不修改原文件。

`result.json` 使用 `agentgo.swe-result/v5`；`judge.json` 使用 `agentgo.swe-judge/v2`。分别保留 Graph/Delivery 结果、执行是否收口、架构检查、provider 兼容性、基础设施状态和 pytest verdict。`architecture_ok=null` 表示当前证据或执行状态不足以完成审计，不能显示为架构通过。最终 resolved 还要求真实测试身份、有补丁、无测试篡改、Graph success 和执行收口。

退出码：0 表示通过；1 为基础设施/结果证据问题或批次未完整执行；2 为已判定的运行契约失败；3 为未修复或执行未完成；4 为 provider 工具/协议兼容性失败。保留具体 reason code 和各项结果，不能只展示一个总分。

## 进程、批次与跨平台约束

monitor 持有 Popen 并用 poll 查询生命周期。Graph terminal 后继续等待当前 Run 的在途任务和 final-report，不能仅因一段时间没有活动便终止。外部超时保留最后快照与未完成事实，再终止并回收进程；不伪造 Observation/no-progress 失败。

每题用跨平台独占锁保护 worktree 与运行产物。batch summary 只消费 `.batch_start` 标记后的当前事务，区分 completed、completed_with_infrastructure_error、infrastructure_error 和 not_run，旧结果不能补齐本批次覆盖率。provider 配额耗尽保留 provider_quota_exhausted 并停止批次。

Windows 路径占位符统一使用正斜杠；只转换 Path，不改写 URL/model/token。清理 disposable worktree 时仅对 Windows ReadOnly PermissionError 清除属性并重试一次，不吞掉占用或其它 I/O 错误。所有长生命周期文件与进程句柄在临时目录清理前关闭。

## 后续验证

CTest 题目可在 `environment.sanitizer` 声明 `address`。这类题目使用 AddressSanitizer 完成基线、红绿资格及最终判题；编译器、版本、CMake 参数和运行选项进入环境身份，缺少编译器或构建失败时停止，不退回普通构建。Linux 使用原生 `c++`；Windows 需在已初始化的 MSVC x64 Developer PowerShell 中启动 Runner，并安装 MSVC AddressSanitizer，测试进程保持普通权限。MSVC 使用异常展开和调试信息，同时排除与 ASan 不兼容的 `/RTC`、增量链接。只有声明该要求的题目使用此路径。

`fmt-8a7aea04222d` 保留原上游测试与修复提交，声明上述环境以可靠检测越界读取；普通 Debug 构建下测试通过不代表缺陷消失。逐题资格与资源证据保存在本地 testbed。

离线 Python 回归位于 `*_test.py`；通用双协议二进制冒烟位于 `../local_fake_provider_smoke.py`，不使用 Flask 或真实 provider。CI 保留 Windows/Linux/macOS。Python 离线单元测试与 Windows/Linux 本地二进制已通过；它们与真实 SWE 是不同证据，验证明细以工具契约第 13 章为准。
