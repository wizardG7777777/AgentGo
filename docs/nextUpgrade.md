# 下一阶段升级计划

## 1. FIFO 队列升级为更灵活的任务管理机制

当前使用简单的 FIFO 队列驱逐已完成任务（`internal/store/memory.go` 的 `addTerminal` 方法），存在以下局限：

- 驱逐不考虑依赖关系，可能删除仍被其他 pending 任务依赖的已完成任务
- 纯按完成顺序驱逐，无法区分任务重要性
- 当前 MVP 阶段通过设置足够大的 fifoLimit（默认 100）来规避，但不是长期方案

### 升级方向建议

- **引用计数 / 依赖感知驱逐**：只有当没有其他任务依赖某个已完成任务时才允许驱逐
- **LRU + 依赖保护**：最近被读取（`GetDependencyResults`）的任务受保护不被驱逐
- **按类型/优先级配置保留策略**：支持为不同任务类型或优先级配置不同的保留时长与数量上限

## 2. 工具路径遍历安全加固

当前 Explorer 代理的只读工具（`internal/explorer/explorer.go` 中的 `toolReadFile`、`toolListFiles`、`toolGrepSearch`）直接使用 LLM 传入的路径调用 `os.ReadFile` / `filepath.Walk`，没有做路径限制。LLM 理论上可以读取系统上任意文件（如 `/etc/passwd`、`~/.ssh/id_rsa` 等）。

### 升级方向建议

- **项目根目录限制**：引入 project root 配置，所有工具路径操作限制在此目录内
- **路径规范化**：对路径进行 `filepath.Clean` + `filepath.Abs` 处理，防止 `../` 跳出
- **可配置白名单/黑名单**：允许管理员指定额外的可访问目录或禁止访问的目录
- **敏感文件模式过滤**：自动拦截对 `.env`、`.ssh`、`credentials` 等敏感文件的访问
