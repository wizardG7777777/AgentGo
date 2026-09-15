# 2026-09-12 Flask 系列共享镜像与外部 AgentGo 二进制

状态：共享镜像的 linux/amd64、linux/arm64 两个变体已本地构建并验证。镜像不包含 AgentGo，
创建测试容器必须显式提供 Linux ELF 二进制。未推送镜像仓库，未运行真实 provider SWE，未使用真实凭据。

## 交付与约束

- 本地镜像名：`agentgo-swe-flask:ubuntu24.04-py3.13-v1`。
- 最终多平台镜像索引：`sha256:dead0a23264c8890bc2a63d9a69d75163b353ff769dfa93524ed48d5fefdb710`。
- 每个变体使用 Ubuntu 24.04、Python 3.13.5、pip 25.1.1，内置 Flask-8、Runner、Prompt 和八套预装 `.venv`。
- 最终镜像中无 Go、uv、gcc、Node、Java；Git/CA 是现有题目准备及 HTTPS 所需系统工具。
- 构建临时 stage 使用 uv 下载 CPython、导出固定 uv.lock；依赖由 pip 安装，uv 和构建用 flit_core 不进入最终运行环境。
- 个人 config.json 仍只做文件存在性门禁，运行时解释内容；不进入镜像、不从宿主环境补填凭据。
- “指定二进制”放在创建测试容器的入口，共享环境镜像独立维护，可测试多个 AgentGo 版本。
  `--agentgo-binary` 不提供就报错；拒绝 PE/Mach-O、损坏 ELF 与架构不匹配。
- 二进制按 SHA256 记录版本，复制到本批卷后复核摘要和执行权限，随后执行 `-h` 检查实际可启动性。
- 默认报告/行为日志导出后删除本批容器与卷，`--persist` 保留停止状态容器及完整内部现场。

## 实际验证

宿主为 Windows amd64，Docker Linux 内核 `6.6.87.2-microsoft-standard-WSL2`。
ARM 验证通过 Docker 仿真进行，不代表 M3 Max 原生运行或 macOS 兼容性测试。
Windows/macOS 仍使用原生环境测试，不新增这两种系统的 Docker 兼容性测试。

验证材料：

```text
${LOCALAPPDATA}/AgentGo/swe-flask-image-validation-20260912/
  final-image.log
  suite-results.json
  verify-amd64.console.log
  verify-arm64.console.log
  artifacts/20260912T123714Z-f00ff90c/
  artifacts/20260912T123754Z-7df4e651/
  artifacts/20260912T125151Z-4ddcaa8c/
```

宿主 Go 1.25.0、CGO_ENABLED=0 构建的验证二进制：

| CPU | SHA256 |
|---|---|
| amd64 | 665dde08285c4c2b6c79f9c88a6fb6745c1d35c841c97a79ff52f564861eb038 |
| arm64 | f0423dfbde977100d322c093bda918a4ae008ef22f0b9097c52a471b7a161f28 |

| 验证 | 结果 |
|---|---|
| amd64 Flask-8 题目红绿自检 | 8/8，通过，40.2 秒 |
| arm64 仿真 Flask-8 题目红绿自检 | 8/8，通过，371.1 秒 |
| amd64 Responses / Chat Completions 真实二进制本地 SSE 冒烟 | 均通过，各 27 次 fixture 调用，Graph success |
| arm64 仿真 Responses 真实二进制本地 SSE 冒烟 | 通过，27 次 fixture 调用，Graph success |
| 封装回归 | Windows、amd64 容器、arm64 容器各 23 项通过 |
| 原 Python Runner 回归 | 70 项通过 |
| Go 测试/vet/构建 | go test ./...、go vet ./...、go build 通过 |
| 默认清理 | 两个架构的自检结束后，本批容器和卷均已删除 |
| 持久化 | 外部二进制＋空配置＋--persist，配置解释失败后容器/卷保留，回执完整 |
| 输入错误 | 缺少二进制、Windows EXE、CPU 架构不匹配均在创建测试容器前明确失败 |

八题自检使用镜像索引 `sha256:260a01bb1798f888dba2c0655ff05772ed7a5ebf1201b740abfbfac1f78d800c`；
随后补全通用二进制冒烟的 Prompt 资源及宿主多平台索引选择回归。
代码/资源更新后的索引 `sha256:8db2ec217ca44767b8c0be3e512514a9e716b281dfbc2f9cde9d5fa6f82a5f40`
完成两个变体的 23 项回归及上述二进制冒烟。最终索引仅再规范 README 的 LF 行尾并重生成镜像身份，
程序、依赖和 Flask 环境未改变。

题目自检确认干净基线、golden tests 红态及 golden source fix 绿态。正式运行阶段没有执行 pip install 或 uv sync。
这些结果不等于 AgentGo 使用真实模型修复八题成功，真实模型修复成绩必须另开批次。

## 构造中发现并解决的问题

1. uv 管理的 CPython 不允许直接向解释器目录安装 pip。改用预建的 `/opt/python-tools/.venv` 提供 Python/pip。
2. 从符号链接路径创建标准 venv 会使 CPython 的 home 指向错误目录并找不到 encodings。
   创建时解析真实解释器路径；复制题目环境时重定位 pip 启动脚本、editable `.pth` 与 direct_url 元数据。
3. Docker containerd 的平台检查返回子 manifest 摘要，它不一定是能直接创建容器的注册镜像 ID。
   现在固定父索引 ID，再带 `--platform` 检查/创建对应变体，分别记录父索引和平台 manifest。
4. create 失败可能只留下已分配的空卷。仅当确认没有本批容器且卷名/标签匹配时删除该卷，避免遗留资源。
5. 内置二进制冒烟还依赖通用角色 Prompt；已一并纳入镜像并增加资源完整性检查。

## 文件对账

新增 binary_input.py；重写 run.py 和 Dockerfile，使维护者构建系列镜像、普通用户只指定外部二进制运行。
prepare_dependencies.py 改为 pip 预装；prebuilt_environment.py 支持标准 venv 路径重定位。
entrypoint.py 固定镜像内测试资源和本批二进制副本，导出 image.json / agentgo-binary.json 作为测试身份。
更新角色/题目 Prompt 为直接调用虚拟环境 Python 的跨平台命令，更新 suite.json、回归、文档和忽略规则。
删除每次测试自动 Go 编译/重建镜像及 --no-build 入口；不迁移历史运行或删除历史产物。

共享发布仍需维护者指定并推送自己的镜像仓库；本次没有自动上传测试资源。
M3 Max 原生 Docker Linux/arm64、真实 provider 修复成绩以及动态链接的其他 AgentGo 二进制仍需各自验证。
