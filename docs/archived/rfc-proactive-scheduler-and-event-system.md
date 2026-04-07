# RFC: Proactive Scheduler & Event-Driven Agent Activation

> Status: Draft
> Authors: —
> Created: 2026-03-23

## 1. 动机与问题陈述

### 1.1 当前架构的局限

deepagents 当前的 Agent 调度基于 **同步阻塞的 `task` tool**（`SubAgentMiddleware`）。主 Agent 通过 LLM 决策调用 `task(description, subagent_type)`，执行路径为：

```
主 Agent LLM → tool call: task("做X", "general-purpose") → subagent.invoke() → 阻塞等待 → 返回结果
```

这导致两个核心问题：

**问题 1：并发度低。** 即使 LLM 在一个 turn 中发出多个 `task` tool call，LangGraph 的 tool executor 仍然逐个串行执行。N 个子任务的总耗时 = 各任务耗时之和。

**问题 2：调度依赖 LLM 决策。** 所有子任务的触发都必须经过主 Agent 的 LLM 推理。LLM 需要显式"想到"要做某件事，才会发出 tool call。这意味着：
- 每一轮协作都需要一次 LLM 调用（成本 + 延迟）
- LLM 无法预见任务执行过程中产生的衍生需求（如"代码改完了需要 review"）
- 复杂的多步协作（A 完成 → 触发 B → B 完成 → 触发 C）需要多轮 LLM turn，每轮都有推理延迟

### 1.2 现有的并发机制及其不足

| 机制 | 位置 | 并发能力 | 不足 |
|---|---|---|---|
| 同步 SubAgent (`task` tool) | `middleware/subagents.py` | 串行阻塞 | 一次只有一个 subagent 在工作 |
| 异步 SubAgent (`start_async_task`) | `middleware/async_subagents.py` | 真并发，非阻塞 | 依赖远程 LangGraph Server 部署，无法本地使用 |
| Template Fan-Out (`Send`) | `templates/_graph.py` | LangGraph 原生并行 | 仅在 template 路由层生效，且需要预先的任务分解 |

### 1.3 目标

引入两个新机制：

1. **Proactive Scheduler** — 将任务提交与执行解耦，支持本地并行执行多个 subagent
2. **Event System** — 允许 Agent 在无需 LLM 决策的情况下，由事件自动触发

两者结合后，多个 Agent 能够**同时工作**且**自动协作**。

---

## 2. 架构总览

```
用户请求
  │
  ▼
主 Agent (LLM 决策)
  │  submit_task() / submit_task_group()
  ▼
┌──────────────────────────────────────────────────────┐
│                  SchedulerMiddleware                   │
│                                                        │
│  ┌────────────┐    ┌───────────┐    ┌──────────────┐  │
│  │  TaskQueue  │───→│ Scheduler │───→│  WorkerPool  │  │
│  │ (优先级+依赖) │    │  (调度逻辑)  │    │ (并发执行)    │  │
│  └────────────┘    └───────────┘    └──────┬───────┘  │
│                                            │          │
│                                     任务完成/失败      │
│                                            │          │
│                                    ┌───────▼───────┐  │
│                                    │   EventBus    │  │
│                                    │ (发布+规则匹配) │  │
│                                    └───────┬───────┘  │
│                                            │          │
│                                  规则匹配 → 新任务     │
│                                            │          │
│                                    回到 TaskQueue     │
└──────────────────────────────────────────────────────┘
  │
  ▼ 结果回流到主 Agent state
```

---

## 3. Proactive Scheduler

### 3.1 概述

Proactive Scheduler 将当前的"提交即阻塞"模型改为"提交-调度-执行-收集"模型。核心变化：

- **提交（submit）** 是非阻塞的，立即返回 task_id
- **执行（execute）** 由 WorkerPool 异步并发管理
- **收集（await）** 可以一次等待多个任务，期间所有任务并行推进

### 3.2 核心数据结构

#### 3.2.1 TaskRecord

```python
class TaskPriority(IntEnum):
    """任务优先级。数值越小，优先级越高。"""
    CRITICAL = 0
    HIGH = 1
    NORMAL = 2
    LOW = 3
    BACKGROUND = 4

class TaskRecord(TypedDict):
    """调度器管理的任务单元。"""
    task_id: str                     # uuid4
    description: str                 # 给 subagent 的完整 prompt
    subagent_type: str               # subagent name（如 "general-purpose"）
    priority: int                    # TaskPriority 枚举值
    dependencies: list[str]          # 依赖的 task_id 列表
    status: str                      # pending | ready | running | success | error | cancelled
    result: str | None               # 完成后的结果文本
    error: str | None                # 失败时的错误信息
    created_at: str                  # ISO-8601
    started_at: str | None
    completed_at: str | None
    metadata: dict[str, Any]         # 可扩展的元数据
```

#### 3.2.2 状态流转

```
                  依赖未满足         依赖满足
  submit ──→ [pending] ──────→ [ready] ──→ [running] ──→ [success]
                                  │            │
                                  │            └──→ [error]
                                  │
                             [cancelled]
```

- `pending`：已提交，但存在未完成的依赖
- `ready`：依赖已满足，等待 Worker 空闲
- `running`：正在由 Worker 执行
- `success` / `error`：终态
- `cancelled`：被用户或系统取消

### 3.3 TaskQueue

```python
class TaskQueue:
    """优先级 + 依赖感知的任务队列。

    排序规则：
    1. priority 数值小的优先
    2. 同优先级下，created_at 早的优先（FIFO）
    """

    def __init__(self):
        self._tasks: dict[str, TaskRecord] = {}
        self._ready_queue: asyncio.PriorityQueue[tuple[int, str, str]] = (
            asyncio.PriorityQueue()
        )
        # 元组格式: (priority, created_at, task_id)

    def submit(self, task: TaskRecord) -> str:
        """提交任务。如果依赖已满足则直接进入 ready 队列。"""
        self._tasks[task["task_id"]] = task
        if self._dependencies_met(task):
            task["status"] = "ready"
            self._ready_queue.put_nowait(
                (task["priority"], task["created_at"], task["task_id"])
            )
        return task["task_id"]

    def _dependencies_met(self, task: TaskRecord) -> bool:
        """检查所有依赖是否已成功完成。"""
        for dep_id in task["dependencies"]:
            dep = self._tasks.get(dep_id)
            if dep is None or dep["status"] != "success":
                return False
        return True

    def on_task_completed(self, task_id: str):
        """任务完成后，检查是否有新任务的依赖被满足。"""
        for t in self._tasks.values():
            if (
                t["status"] == "pending"
                and task_id in t["dependencies"]
                and self._dependencies_met(t)
            ):
                t["status"] = "ready"
                self._ready_queue.put_nowait(
                    (t["priority"], t["created_at"], t["task_id"])
                )

    async def get_next(self) -> TaskRecord:
        """获取下一个 ready 任务（阻塞直到有可用任务）。"""
        _, _, task_id = await self._ready_queue.get()
        return self._tasks[task_id]

    def cancel(self, task_id: str) -> bool:
        """取消一个 pending 或 ready 的任务。"""
        task = self._tasks.get(task_id)
        if task and task["status"] in ("pending", "ready"):
            task["status"] = "cancelled"
            return True
        return False
```

### 3.4 WorkerPool

```python
class WorkerPool:
    """基于 asyncio.Semaphore 的并发执行池。

    Args:
        max_workers: 最大并发 worker 数。每个 worker 运行一个 subagent。
    """

    def __init__(self, max_workers: int = 4):
        self._sem = asyncio.Semaphore(max_workers)
        self._active: dict[str, asyncio.Task] = {}

    @property
    def active_count(self) -> int:
        return len(self._active)

    @property
    def available_count(self) -> int:
        # Semaphore 的内部计数器反映可用 slot
        return self._sem._value

    async def execute(
        self,
        task: TaskRecord,
        subagent: Runnable,
        base_state: dict[str, Any],
    ) -> str:
        """在控制并发的前提下执行一个 subagent。

        Args:
            task: 要执行的任务记录
            subagent: 编译好的 subagent runnable
            base_state: 从主 agent state 中提取的基础状态（排除 messages 等）

        Returns:
            subagent 最终消息的文本内容
        """
        async with self._sem:
            task["status"] = "running"
            task["started_at"] = _now()

            subagent_state = {
                **base_state,
                "messages": [HumanMessage(content=task["description"])],
            }

            try:
                result = await subagent.ainvoke(subagent_state)
                task["status"] = "success"
                task["result"] = result["messages"][-1].text.rstrip()
            except Exception as e:
                task["status"] = "error"
                task["error"] = str(e)
                raise
            finally:
                task["completed_at"] = _now()
                self._active.pop(task["task_id"], None)

            return task["result"]

    async def cancel(self, task_id: str) -> bool:
        """取消一个正在运行的任务。"""
        asyncio_task = self._active.get(task_id)
        if asyncio_task:
            asyncio_task.cancel()
            return True
        return False
```

### 3.5 Scheduler（调度循环）

```python
class Scheduler:
    """从 TaskQueue 取任务，分发到 WorkerPool，处理完成回调。

    Scheduler 作为一个后台 asyncio.Task 持续运行，
    直到所有任务完成或被显式停止。
    """

    def __init__(
        self,
        queue: TaskQueue,
        pool: WorkerPool,
        subagent_graphs: dict[str, Runnable],
        base_state: dict[str, Any],
        event_bus: EventBus | None = None,
    ):
        self._queue = queue
        self._pool = pool
        self._subagent_graphs = subagent_graphs
        self._base_state = base_state
        self._event_bus = event_bus
        self._running = False

    async def start(self):
        """启动调度循环。"""
        self._running = True
        while self._running:
            task = await self._queue.get_next()

            if task["status"] == "cancelled":
                continue

            subagent = self._subagent_graphs.get(task["subagent_type"])
            if subagent is None:
                task["status"] = "error"
                task["error"] = f"Unknown subagent type: {task['subagent_type']}"
                continue

            # 不 await —— 让任务在后台运行，调度器继续取下一个任务
            asyncio_task = asyncio.create_task(
                self._run_and_handle(task, subagent)
            )
            self._pool._active[task["task_id"]] = asyncio_task

    async def _run_and_handle(self, task: TaskRecord, subagent: Runnable):
        """执行任务并处理完成后的后续逻辑。"""
        try:
            await self._pool.execute(task, subagent, self._base_state)
        except Exception:
            pass  # execute() 内部已更新 task status
        finally:
            # 1. 通知 TaskQueue 检查依赖解锁
            self._queue.on_task_completed(task["task_id"])

            # 2. 发布事件到 EventBus
            if self._event_bus:
                await self._event_bus.emit(AgentEvent(
                    event_type="task_completed",
                    source=task["subagent_type"],
                    timestamp=_now(),
                    payload={
                        "task_id": task["task_id"],
                        "subagent_type": task["subagent_type"],
                        "status": task["status"],
                        "result_summary": (task.get("result") or "")[:500],
                    },
                ))

    async def stop(self):
        """优雅停止调度循环。"""
        self._running = False

    async def await_tasks(self, task_ids: list[str], timeout: float | None = None) -> dict[str, TaskRecord]:
        """等待指定的任务全部到达终态。

        不是轮询——通过 asyncio.Event 实现高效等待。

        Returns:
            task_id → TaskRecord 的映射
        """
        # 实现省略，核心是为每个 task 关联一个 asyncio.Event，
        # 在 _run_and_handle 完成时 set()
        ...
```

### 3.6 提供给 LLM 的 Tools

SchedulerMiddleware 向主 Agent 暴露以下 tools：

#### `submit_task`
```
提交单个任务到调度队列。立即返回 task_id，不阻塞。

参数:
  description: str      — 任务的详细描述
  subagent_type: str    — 使用哪个 subagent
  priority: str         — "critical" | "high" | "normal" | "low" | "background"
  dependencies: list[str] — 依赖的 task_id 列表（可选）

返回:
  task_id: str
```

#### `submit_task_group`
```
批量提交一组任务，支持在组内声明依赖关系。

参数:
  tasks: list[{description, subagent_type, priority, depends_on_index}]
    — depends_on_index 引用本组内其他任务的索引（0-based）

返回:
  task_ids: list[str]

示例:
  submit_task_group(tasks=[
    {description: "切菜", subagent_type: "chef", priority: "normal"},                    # index 0
    {description: "烧水", subagent_type: "chef", priority: "normal"},                    # index 1
    {description: "炒菜（需要切好的菜）", subagent_type: "chef", depends_on_index: [0]},   # index 2
    {description: "下面（需要水烧开）", subagent_type: "chef", depends_on_index: [1]},     # index 3
    {description: "摆盘（需要炒菜和面都好）", subagent_type: "chef", depends_on_index: [2, 3]}, # index 4
  ])
  → 0,1 立即并行执行；2 等 0 完成；3 等 1 完成；4 等 2,3 都完成
```

#### `await_tasks`
```
等待一组任务全部完成。在等待期间，所有已提交的任务继续并行推进。

参数:
  task_ids: list[str]   — 要等待的 task_id 列表
  timeout: float | None — 超时秒数（可选）

返回:
  results: dict[str, {status, result, error}]
```

#### `get_task_status`
```
查询一个或多个任务的当前状态。非阻塞。

参数:
  task_ids: list[str]

返回:
  statuses: dict[str, {status, result, error, created_at, started_at, completed_at}]
```

#### `cancel_task`
```
取消一个尚未完成的任务。

参数:
  task_id: str

返回:
  success: bool
```

### 3.7 与现有代码的集成

SchedulerMiddleware 作为 `SubAgentMiddleware` 的替代品（或增强模式），在 `graph.py` 中的位置：

```python
# graph.py 中现有代码
SubAgentMiddleware(
    backend=backend,
    subagents=inline_subagents,
)

# 替换为（向后兼容）
SubAgentMiddleware(
    backend=backend,
    subagents=inline_subagents,
    scheduling="proactive",   # 新参数，默认 "blocking" 保持向后兼容
    max_workers=4,            # 新参数，仅 proactive 模式有效
)
```

内部实现：
- `scheduling="blocking"` → 构建当前的 `task` tool（行为不变）
- `scheduling="proactive"` → 构建 `submit_task` / `submit_task_group` / `await_tasks` / `get_task_status` / `cancel_task`

subagent 的编译和解析逻辑（`_get_subagents()` / `_get_subagents_legacy()`）完全复用，不需要修改。

---

## 4. Event System

### 4.1 概述

Event System 是一个发布-订阅机制，允许系统中的各个组件（middleware、scheduler、tools）在特定事件发生时发布事件，EventBus 根据预定义的规则自动触发相应的 Agent 行为。

核心原则：**事件触发的 Agent 不需要主 Agent 的 LLM 参与决策**。规则在初始化时声明，运行时自动匹配执行。

### 4.2 核心数据结构

#### 4.2.1 AgentEvent

```python
class AgentEvent(TypedDict):
    """系统中传播的事件。"""
    event_type: str           # 事件类型标识符
    source: str               # 产生事件的组件名
    timestamp: str            # ISO-8601
    payload: dict[str, Any]   # 事件携带的数据
```

#### 4.2.2 内置事件类型

| event_type | 触发时机 | payload 字段 |
|---|---|---|
| `task_completed` | subagent 任务完成 | `task_id`, `subagent_type`, `status`, `result_summary` |
| `task_failed` | subagent 任务失败 | `task_id`, `subagent_type`, `error` |
| `file_changed` | `write_file` 或 `edit_file` 被调用 | `path`, `operation` ("write"\|"edit"), `agent_name` |
| `file_created` | `write_file` 创建新文件 | `path`, `agent_name` |
| `execution_completed` | `execute` tool 完成 | `command`, `exit_code`, `agent_name` |
| `timeout` | 任务执行超过阈值 | `task_id`, `elapsed_seconds`, `threshold` |
| `state_changed` | agent state 中指定 key 发生变化 | `key`, `old_value`, `new_value` |

用户也可以定义自定义事件类型。

#### 4.2.3 EventRule

```python
class EventRule(TypedDict):
    """声明式事件规则：当事件匹配时，自动激活指定 agent。"""

    name: str
    """规则名称，用于日志和调试。"""

    event_type: str
    """匹配的事件类型。"""

    condition: NotRequired[str]
    """可选的过滤条件。

    简单的 Python 表达式，可引用 payload 中的字段。
    示例:
      - "path.endswith('.py')"
      - "status == 'error'"
      - "elapsed_seconds > 60"
    不提供时，匹配所有该类型的事件。
    """

    activate_agent: str
    """要激活的 subagent name。"""

    prompt_template: str
    """给被激活 agent 的 prompt 模板。

    支持 {field_name} 占位符，从 event payload 中填充。
    示例: "Review the changes to {path}. Focus on correctness."
    """

    priority: NotRequired[int]
    """触发的任务优先级。默认 TaskPriority.NORMAL (2)。"""

    debounce_seconds: NotRequired[float]
    """防抖间隔。同一规则在此时间窗口内只触发一次。默认 0（不防抖）。

    用途：避免短时间内大量文件修改导致 reviewer 被反复触发。
    """

    max_concurrent: NotRequired[int]
    """此规则同时运行的最大任务数。默认无限制。

    用途：防止事件风暴导致某类 agent 占满 WorkerPool。
    """

    enabled: NotRequired[bool]
    """是否启用此规则。默认 True。可用于运行时动态开关。"""
```

### 4.3 EventBus

```python
class EventBus:
    """事件总线：接收事件，匹配规则，触发 agent。

    EventBus 不直接执行 agent，而是将匹配的任务提交到 Scheduler 的 TaskQueue，
    由 Scheduler 统一调度。这确保了事件触发的任务与手动提交的任务共享同一个
    WorkerPool 和优先级系统。
    """

    def __init__(
        self,
        rules: list[EventRule],
        task_queue: TaskQueue,
    ):
        self._rules = [r for r in rules if r.get("enabled", True)]
        self._task_queue = task_queue
        self._last_triggered: dict[str, float] = {}  # rule_name → timestamp
        self._active_counts: dict[str, int] = {}     # rule_name → running count
        self._listeners: list[Callable[[AgentEvent], Awaitable[None]]] = []

    async def emit(self, event: AgentEvent):
        """发布事件。同步匹配规则并提交任务。

        Args:
            event: 要发布的事件
        """
        for rule in self._rules:
            if not self._matches(rule, event):
                continue
            if self._is_debounced(rule):
                continue
            if self._exceeds_max_concurrent(rule):
                continue

            prompt = self._render_prompt(rule, event)
            task = TaskRecord(
                task_id=str(uuid4()),
                description=prompt,
                subagent_type=rule["activate_agent"],
                priority=rule.get("priority", TaskPriority.NORMAL),
                dependencies=[],
                status="pending",
                result=None,
                error=None,
                created_at=_now(),
                started_at=None,
                completed_at=None,
                metadata={
                    "triggered_by_event": event["event_type"],
                    "triggered_by_rule": rule["name"],
                    "event_source": event["source"],
                },
            )
            self._task_queue.submit(task)
            self._last_triggered[rule["name"]] = time.monotonic()

        # 通知额外的监听器（用于日志、指标等）
        for listener in self._listeners:
            await listener(event)

    def _matches(self, rule: EventRule, event: AgentEvent) -> bool:
        """检查事件是否匹配规则。"""
        if rule["event_type"] != event["event_type"]:
            return False
        condition = rule.get("condition")
        if condition is None:
            return True
        # 在 event payload 的上下文中安全求值
        return _safe_eval(condition, event["payload"])

    def _is_debounced(self, rule: EventRule) -> bool:
        """检查规则是否在防抖窗口内。"""
        debounce = rule.get("debounce_seconds", 0)
        if debounce <= 0:
            return False
        last = self._last_triggered.get(rule["name"], 0)
        return (time.monotonic() - last) < debounce

    def _exceeds_max_concurrent(self, rule: EventRule) -> bool:
        """检查规则是否达到最大并发数。"""
        max_concurrent = rule.get("max_concurrent")
        if max_concurrent is None:
            return False
        return self._active_counts.get(rule["name"], 0) >= max_concurrent

    def _render_prompt(self, rule: EventRule, event: AgentEvent) -> str:
        """用事件 payload 填充 prompt 模板。"""
        return rule["prompt_template"].format(**event["payload"])

    def add_listener(self, listener: Callable[[AgentEvent], Awaitable[None]]):
        """注册额外的事件监听器（用于日志、监控等）。"""
        self._listeners.append(listener)
```

### 4.4 事件源埋点

事件从现有的 middleware 和 tool 内部产生。需要修改的位置：

#### 4.4.1 FilesystemMiddleware (`middleware/filesystem.py`)

在 `write_file` 和 `edit_file` 的执行逻辑完成后插入：

```python
# write_file 成功后
if event_bus:
    file_existed = ...  # 之前是否存在
    await event_bus.emit(AgentEvent(
        event_type="file_created" if not file_existed else "file_changed",
        source="filesystem",
        timestamp=_now(),
        payload={
            "path": file_path,
            "operation": "write",
            "agent_name": current_agent_name,
        },
    ))

# edit_file 成功后
if event_bus:
    await event_bus.emit(AgentEvent(
        event_type="file_changed",
        source="filesystem",
        timestamp=_now(),
        payload={
            "path": file_path,
            "operation": "edit",
            "agent_name": current_agent_name,
        },
    ))
```

#### 4.4.2 Scheduler (`_run_and_handle` 中)

已在 3.5 节的 `Scheduler._run_and_handle` 中展示。任务完成和失败都会产生事件。

#### 4.4.3 execute tool (`middleware/filesystem.py`)

```python
# execute 完成后
if event_bus:
    await event_bus.emit(AgentEvent(
        event_type="execution_completed",
        source="filesystem",
        timestamp=_now(),
        payload={
            "command": command,
            "exit_code": exit_code,
            "agent_name": current_agent_name,
        },
    ))
```

#### 4.4.4 超时检测（WorkerPool 内部）

```python
# 可选：通过一个后台 monitor task 定期检查
async def _monitor_timeouts(self, timeout_threshold: float):
    while self._running:
        await asyncio.sleep(5)  # 每 5 秒检查一次
        now = time.monotonic()
        for task_id, task in self._queue._tasks.items():
            if task["status"] == "running" and task["started_at"]:
                elapsed = now - _parse_timestamp(task["started_at"])
                if elapsed > timeout_threshold:
                    await self._event_bus.emit(AgentEvent(
                        event_type="timeout",
                        source="scheduler",
                        timestamp=_now(),
                        payload={
                            "task_id": task_id,
                            "elapsed_seconds": elapsed,
                            "threshold": timeout_threshold,
                        },
                    ))
```

### 4.5 事件结果回流

事件触发的任务完成后，结果需要被主 Agent 感知到。支持两种模式：

#### 4.5.1 静默模式（默认）

结果写入 agent state 的 `event_results` 字段。主 Agent 在下一次 LLM turn 时通过系统消息看到新的事件结果。

```python
class SchedulerState(AgentState):
    """Scheduler 扩展的 agent state。"""
    scheduler_tasks: Annotated[
        dict[str, TaskRecord],
        _scheduler_tasks_reducer,
    ]
    event_results: Annotated[
        list[EventResult],
        _event_results_reducer,
    ]

class EventResult(TypedDict):
    rule_name: str
    event_type: str
    agent_name: str
    task_id: str
    result: str
    timestamp: str
```

SchedulerMiddleware 在 `wrap_model_call` 中将未读的 `event_results` 注入系统消息：

```python
def wrap_model_call(self, request, handler):
    unread = self._get_unread_event_results(request.state)
    if unread:
        summary = self._format_event_results(unread)
        new_system = append_to_system_message(
            request.system_message,
            f"\n\n## Background task results\n{summary}"
        )
        return handler(request.override(system_message=new_system))
    return handler(request)
```

#### 4.5.2 中断模式

对于关键事件（如发现安全漏洞），通过 LangGraph 的 `interrupt` 机制暂停主 Agent：

```python
# EventRule 中可以指定
class EventRule(TypedDict):
    ...
    interrupt: NotRequired[bool]
    """是否在事件触发时中断主 Agent。默认 False。"""
```

---

## 5. 两者结合的工作流

### 5.1 完整数据流示例

以"重构 3 个 Python 文件"为例：

```
Step 1: 主 Agent 收到用户请求
  │
  ▼ LLM 决策：调用 submit_task_group
Step 2: submit_task_group([
           {description: "重构 A.py", agent: "refactor"},  # → task_0
           {description: "重构 B.py", agent: "refactor"},  # → task_1
           {description: "重构 C.py", agent: "refactor"},  # → task_2
         ])
  │ 立即返回 [task_0, task_1, task_2]
  │
  │ ┌─────── Scheduler 并行执行 ───────┐
  │ │ Worker 1: 重构 A.py               │
  │ │ Worker 2: 重构 B.py               │
  │ │ Worker 3: 重构 C.py               │
  │ └─────────────────────────────────┘
  │
  ▼ LLM 决策：调用 await_tasks([task_0, task_1, task_2])
Step 3: 等待期间，task_0 先完成
  │
  │ ── task_0 完成 ──→ Scheduler 发布事件 ──→ EventBus 匹配规则:
  │                     {event: "task_completed",
  │                      condition: "subagent_type == 'refactor'",
  │                      activate: "reviewer",
  │                      prompt: "Review changes from refactoring: {result_summary}"}
  │                                    │
  │                                    ▼
  │                     EventBus 提交新任务到 TaskQueue
  │                     → task_3: Review A.py (priority: NORMAL)
  │
  │ ── task_1 完成 ──→ 同上 → task_4: Review B.py
  │ ── task_2 完成 ──→ 同上 → task_5: Review C.py
  │
  │ ── task_3 完成 ──→ event_results 写入 state
  │ ── task_4, task_5 完成
  │
  ▼
Step 4: await_tasks 返回 task_0/1/2 的结果
  │
  ▼ 主 Agent 下一次 LLM turn
Step 5: 系统消息中出现 event_results:
         "Background task results:
          - [reviewer] Review A.py: LGTM, no issues
          - [reviewer] Review B.py: Found unused import on line 42
          - [reviewer] Review C.py: LGTM"
  │
  ▼ LLM 决策：修复 B.py 的问题
Step 6: submit_task({description: "Fix unused import in B.py line 42", agent: "refactor"})
```

### 5.2 链式反应示例

事件可以触发新任务，新任务完成后又产生新事件，形成链式反应：

```
事件规则配置:
  Rule 1: file_changed + path.endswith('.py') → activate reviewer
  Rule 2: task_completed + subagent_type=='reviewer' + 'issues' in result → activate fixer
  Rule 3: task_completed + subagent_type=='fixer' → activate tester

执行流:
  Worker 修改了 foo.py
    → [Rule 1] EventBus 触发 reviewer 审查 foo.py
      → reviewer 发现问题
        → [Rule 2] EventBus 触发 fixer 修复 foo.py
          → fixer 修复完成
            → [Rule 3] EventBus 触发 tester 测试 foo.py
```

### 5.3 安全机制

链式反应需要防止无限循环和资源耗尽：

| 机制 | 作用 |
|---|---|
| `max_concurrent` | 每条规则的最大并发任务数 |
| `debounce_seconds` | 防抖，避免高频事件风暴 |
| `max_chain_depth` (全局) | 事件链最大深度，防止无限循环。默认 5 |
| `max_event_tasks` (全局) | 事件触发的任务总数上限。默认 20 |
| WorkerPool `max_workers` | 总并发上限，无论手动提交还是事件触发 |

```python
class SchedulerConfig(TypedDict):
    """调度器全局配置。"""
    max_workers: int               # 默认 4
    max_chain_depth: int           # 默认 5
    max_event_tasks: int           # 默认 20
    task_timeout_seconds: float    # 默认 300 (5 分钟)
```

---

## 6. 用户层 API

### 6.1 create_deep_agent 扩展

```python
agent = create_deep_agent(
    model="anthropic:claude-sonnet-4-6",
    subagents=[
        {"name": "refactor", "description": "Code refactoring", ...},
        {"name": "reviewer", "description": "Code review", ...},
        {"name": "tester",   "description": "Write tests", ...},
    ],

    # 新参数
    scheduling="proactive",
    scheduler_config={
        "max_workers": 4,
        "max_chain_depth": 5,
        "max_event_tasks": 20,
        "task_timeout_seconds": 300,
    },
    event_rules=[
        {
            "name": "auto-review-on-file-change",
            "event_type": "file_changed",
            "condition": "path.endswith('.py')",
            "activate_agent": "reviewer",
            "prompt_template": "Review the changes to {path}. Focus on correctness and style.",
            "debounce_seconds": 5.0,
        },
        {
            "name": "auto-test-on-review-pass",
            "event_type": "task_completed",
            "condition": "subagent_type == 'reviewer' and 'LGTM' in result_summary",
            "activate_agent": "tester",
            "prompt_template": "Write unit tests for the recently reviewed code.",
            "priority": 3,
        },
    ],
)
```

### 6.2 独立使用 EventBus（无 Scheduler）

EventBus 也可以脱离 Scheduler 独立使用，作为纯粹的事件通知系统：

```python
from deepagents.events import EventBus, EventRule

bus = EventBus(rules=[...], task_queue=None)

# 仅注册监听器，不自动触发 agent
bus.add_listener(my_logging_handler)
bus.add_listener(my_metrics_handler)
```

### 6.3 独立使用 Scheduler（无 EventBus）

```python
SubAgentMiddleware(
    backend=backend,
    subagents=inline_subagents,
    scheduling="proactive",
    max_workers=4,
    # 不传 event_rules → 无 EventBus，纯调度器模式
)
```

---

## 7. 文件结构

```
libs/deepagents/deepagents/
├── middleware/
│   ├── subagents.py             # 现有，添加 scheduling 参数
│   ├── async_subagents.py       # 现有，不修改
│   ├── scheduler.py             # 新增：TaskQueue, WorkerPool, Scheduler, SchedulerMiddleware
│   ├── events.py                # 新增：AgentEvent, EventRule, EventBus
│   └── filesystem.py            # 修改：在 write/edit/execute 中添加事件埋点
├── graph.py                     # 修改：支持 scheduling, scheduler_config, event_rules 参数
└── ...
```

---

## 8. 向后兼容性

| 场景 | 行为 |
|---|---|
| 不传 `scheduling` 参数 | 默认 `"blocking"`，行为与当前完全一致 |
| 不传 `event_rules` 参数 | 无 EventBus，不会有事件触发 |
| `scheduling="proactive"` 但不传 `event_rules` | 纯调度器模式，LLM 通过 submit/await 手动管理 |
| 现有的 `AsyncSubAgentMiddleware` | 不受影响，继续支持远程 LangGraph Server |
| 现有的 Template Fan-Out | 不受影响，Template 层独立于 middleware 层 |

---

## 9. 实现阶段

| 阶段 | 内容 | 依赖 | 预估改动 |
|---|---|---|---|
| **Phase 1** | `TaskQueue` + `WorkerPool` + `Scheduler` 核心类 | 无 | 新文件 `scheduler.py` |
| **Phase 2** | `SchedulerMiddleware` + tools (submit/await/status/cancel) | Phase 1 | 新文件 `scheduler.py` 扩展 |
| **Phase 3** | `AgentEvent` + `EventRule` + `EventBus` | 无 | 新文件 `events.py` |
| **Phase 4** | EventBus 与 Scheduler 集成 | Phase 1-3 | `scheduler.py` 修改 |
| **Phase 5** | FilesystemMiddleware 事件埋点 | Phase 3 | `filesystem.py` 小改 |
| **Phase 6** | `create_deep_agent` API 扩展 | Phase 1-4 | `graph.py` + `subagents.py` 修改 |
| **Phase 7** | 安全机制（chain depth, max tasks, debounce） | Phase 4 | `events.py` + `scheduler.py` |
| **Phase 8** | 测试 + 文档 | All | tests/ + docstrings |

---

## 10. 开放问题

1. **condition 表达式的安全性**：EventRule 的 `condition` 字段使用简单 Python 表达式。需要决定是用 `ast.literal_eval` 的受限求值，还是引入专用的表达式语言（如 jmespath）。

2. **state 共享策略**：事件触发的 agent 是否应该继承主 Agent 的完整 state？还是像当前 subagent 一样只继承非排除的 key？

3. **结果合并冲突**：如果事件触发的 reviewer agent 和手动提交的 refactor agent 同时修改同一个文件，如何处理冲突？

4. **持久化**：TaskQueue 中的任务是否需要持久化到 checkpointer？如果主 Agent 中断后恢复，未完成的事件任务如何处理？

5. **可观测性**：是否需要为 Scheduler 和 EventBus 提供 LangSmith tracing 集成，使得事件链可以在 trace 中可视化？
