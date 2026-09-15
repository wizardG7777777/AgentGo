# SWE / Flask 系列测试镜像

这个镜像固定提供 Ubuntu 24.04、Python 3.13.5、pip 25.1.1、Flask-8 题库、每题预装的 .venv、
正式 Runner、角色 Prompt、配置模板、行为日志与报告导出代码。**镜像不包含待测 AgentGo 二进制**。
用户只提供待测 Linux 二进制与个人 config.json；不再把宿主脚本、Prompt 或源码目录挂载进测试容器。
一个镜像版本对应 Flask 测试系列，未来其他系列使用独立镜像。

## 普通使用者

宿主只需要 Python 3.13 与 Docker Desktop / Docker Engine（多架构镜像检查需要 API 1.49+）。
Go 工具链只在你自行编译待测程序时需要；启动脚本不再调用 Go，也不在每次测试前构建镜像。

先复制公开模板 `config.json.example` 为本地 `config.json`，自行填写，真实配置不提交 Git。
配置门禁仍只检查文件存在，空白/错误 JSON 不预先校验；进入容器后自然解释并记录错误。

```powershell
Copy-Item SWE_LinuxContainers/config.json.example SWE_LinuxContainers/config.json
# 填写 config.json，然后指定现成的 Linux 二进制。
py -3.13 SWE_LinuxContainers/run.py --agentgo-binary C:/path/to/agentgo-linux-amd64

# 保留停止状态的容器和完整现场。
py -3.13 SWE_LinuxContainers/run.py --agentgo-binary C:/path/to/agentgo-linux-amd64 --persist
```

M3 Max 的 macOS 宿主使用 Linux arm64 二进制：

```sh
python3 SWE_LinuxContainers/run.py --agentgo-binary /path/to/agentgo-linux-arm64
```

缺少二进制、传入 Windows PE / macOS Mach-O、损坏 ELF 或 CPU 架构不匹配都会在创建测试容器前报错。
这里把“构建容器必须指定二进制”落实为创建/启动测试容器时的必选输入；共享测试环境镜像独立制作，
可以重复测试多个 AgentGo 版本。二进制 SHA256 是本次受测版本的精确身份；不靠文件名或可变版本标签猜测。

默认使用 Docker Engine 的原生 CPU 架构；在 amd64 主机上显式 `--arch arm64` 才选择 ARM 容器仿真。
程序会优先使用本地匹配架构的镜像，缺失时按 `--image` 拉取。默认本地镜像名是：
`agentgo-swe-flask:ubuntu24.04-py3.13-v1`。公开镜像为
`wizzardagentgo/agentgo-swe-flask:ubuntu24.04-py3.13-v1`，可追加
`--image wizzardagentgo/agentgo-swe-flask:ubuntu24.04-py3.13-v1` 使用。
2026-09-15 核对的公共索引为 `sha256:6939d7362b1f66b5e1be84df7177895b264e5a21b3a055ae2e4af3777041472e`，
包含 linux/amd64 与 linux/arm64。使用 `--image <仓库/镜像@sha256:摘要>` 可固定镜像身份。
源码更新不会更新已发布镜像；镜像内资源版本以其 `image.json` 为准。

二进制先只读挂载，再复制到本批 Linux 数据卷中，赋执行权限并复核 SHA256，整个批次使用该冻结副本。
读取配置后会执行一次 AgentGo `-h` 启动检查；动态加载器/共享库缺失等问题在真实 provider 探针前记录。
ELF/架构检查不宣称任意旧 AgentGo 版本都兼容当前 Runner 协议，实际协议错误仍由运行结果报告。

## 自行编译待测二进制（容器外）

Windows PowerShell，为当前 Windows/amd64 电脑上的 Linux 容器生成程序：

```powershell
$env:GOOS='linux'
$env:GOARCH='amd64'
$env:CGO_ENABLED='0'
go build -o agentgo-linux-amd64 .
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
```

M3 Max / macOS，在宿主生成 Linux arm64 程序：

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o agentgo-linux-arm64 .
```

也可以使用 CI/其他机器已经构建好的二进制。Windows 和 macOS 自身的兼容性继续在原生系统测试，
不使用 Windows/macOS Docker 镜像。Ubuntu 镜像提供用户空间，内核由 Docker 的 Linux VM/宿主提供；
WSL2 或 macOS Docker VM 的内核不会因为 FROM ubuntu:24.04 就变成独立的 Ubuntu 内核。

## 镜像维护者

普通用户不需要执行本节。维护者只在测试工具、题库、Prompt 或依赖发生变化时构建新镜像。
现有“config.json 文件必须存在”的制作门禁仍保留，可以是空文件；镜像不读取其内容，也不包含凭据。

```powershell
# 本地构建并加载同时包含 amd64 / arm64 的镜像。
py -3.13 SWE_LinuxContainers/run.py build-image --arch all

# 只构建一种 CPU 架构。
py -3.13 SWE_LinuxContainers/run.py build-image --arch amd64

# 已登录自己的镜像仓库后，明确发布多架构版本。
py -3.13 SWE_LinuxContainers/run.py build-image --arch all --image <自己的仓库/镜像:版本> --push
```

使用同一镜像引用的多平台 manifest，Docker 按平台选择对应的 Python、原生依赖 wheel 和 .venv。
构建支持 Docker Desktop 自带仿真；ARM 仿真较慢，M3 Max 上原生构建/运行可避免这部分开销。
旧版 `build`、默认自动编译 Go/重建镜像、`--no-build` 入口已移除，不静默回退到旧测试镜像。

最终容器的语言/包管理环境只有 Python 3.13 + pip，不安装 Go、uv、Node、Java 或编译器。
Git 和 CA 证书是 Flask 题目准备、隔离 Git 工作区和 HTTPS 所需的系统工具，不能从现有 Runner 中删掉。
Docker 构建的临时 stage 使用 uv 下载 CPython、将题库现有 uv.lock 导出为带 hash 的 pip requirements；
uv 不复制到最终镜像。pip 只在镜像制作期间安装各题依赖及 editable Flask，构建用 flit_core 随后移除。

```text
/opt/agentgo/                       镜像内的 Runner、Prompt、配置模板、封装代码、image.json
/opt/python-tools/.venv/            镜像内 Python/pip 工具环境
/opt/swe/upstream/flask/             固定题目所需 Git 对象
/opt/swe/environments/<题目>/.venv/  每题预装的 Python 依赖
/opt/swe/environments/<题目>/manifest.json
/run/swe/agentgo                    用户提供的二进制只读挂载
/run/swe/config.json                个人配置只读挂载
/data/<批次>/bin/agentgo            已核对摘要的本批二进制副本
/data/<批次>/testbed/worktrees/     每题实际运行的源码、.venv 与状态
```

镜像制作通过解析真实 CPython 路径创建 venv，避免解释器链接路径导致找不到标准库。
正式测试只复制每题 .venv、修正 pip 启动脚本和 editable Flask 导入路径并核对依赖身份，
不执行 pip install、uv sync 或 Go 编译。每题锁定依赖不同，避免合成一套环境改变原八题的基线。
正式 pytest 使用 `.venv/bin/python -m pytest`；Windows 原生测试使用 `.venv/Scripts/python.exe -m pytest`。

## 配置、日志与生命周期

个人 JSON 中 `environment` 提供模型与协议，`runner_args` 选择 `batch` / `task` / `verify-candidates`。
镜像固定 Flask-8 的题目路径和内部工作路径，角色模型仍由镜像内 setting.swe-flask.yaml 渲染决定。
不继承宿主旧 SWE 凭据，不自动验证、修正或切换用户填写的模型、地址、密钥。
既有四变量校验和 Python / AgentGo 能力探针继续工作，不新增 L1 负向测试模式。

默认一批一个容器，八题串行，每题一个 AgentGo 进程。结束后仅导出报告和行为日志到
`SWE_LinuxContainers/artifacts/<批次ID>/`，宿主逐文件复核后删除本批容器和独立数据卷。
`--persist` 保留停止状态的容器及完整内部现场；导出范围仍然相同。

导出包括 image.json、agentgo-binary.json、运行/退出报告、result/judge、pytest/JUnit、
Python probe 原文、Graph/Context/Loop/Outcome、Session 模型输出、Trace、Prompt dump 和日志引用正文。
不独立导出配置/完整环境、二进制、Git 工作树、.venv 或候选源码；原文日志本身可能含源码或敏感内容。
archive-status.json 保存导出文件摘要；cleanup.json 保存清理/保留事实。导出完成不等于 SWE 修复成功。

导出失败或校验失败时返回错误并保留尚存现场，避免丢失唯一日志。只清理带本批归属标签的资源，
不使用 prune，不删除历史批次、旧共享卷或无关容器。临时恢复导出容器使用 --rm。
保留的容器不自动续跑；再次测试请创建新批次。宿主自身崩溃的后续清理由人工依据 host.json 核对处理。

宿主 .build 现在只在制作测试镜像时暂存镜像资源，不再为每次测试复制 AgentGo 源码或编译程序。
打包使用明确的源码/题库清单；真实配置、日志和临时目录不读取、不计算摘要、不进入构建上下文。
公开 `config.json.example` 仅含空占位值；真实配置、`.env`、凭据、`.build/`、`artifacts/`、
测试工作区、探针日志、镜像归档和待测二进制均不提交 Git。完整行为日志只保留在本地。
以前的构建暂存产物不自动迁移或删除。compose.yaml 仅供显式 persist profile 的手工排障，
需自行提供 config.json 和 agentgo-linux 文件；日常使用以上 Python 入口。

## 验证与变更对账

```powershell
py -3.13 -m unittest discover -s SWE_LinuxContainers -p 'test_*.py'
py -3.13 -m unittest discover -s scripts/swe_test_runner -p '*_test.py'
go test ./...
go vet ./...
go build
```

新增 binary_input.py 的 ELF/架构/版本身份检查。重写镜像构建和启动入口，外置二进制，所有测试材料内置。
依赖制作改为 pip，运行期只复制镜像内 .venv；更新题目/角色 Prompt 的跨平台测试命令和对应回归。
报告/日志导出、默认清理和 --persist 语义保留。无历史状态迁移，无 Windows/macOS 容器兼容性测试。
真实 provider SWE 成绩、M3 Max 原生 ARM 运行结果须单独记录，不能用仿真或题目红绿自检代替。
