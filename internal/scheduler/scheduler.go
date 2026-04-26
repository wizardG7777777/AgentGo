package scheduler

import (
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/webtool"

	"github.com/google/uuid"
)

// Mode 表示调度器的工作模式（即时 vs 计划）。
//
// Phase 3 重构后，scheduler 不再有自己的事件循环和 currentBatch 字段。
// Mode 现在由 ModeStore 持有，CLI 通过 *ModeStore 切换；SchedulerExecutor
// 在每次注入 board snapshot 时从 ModeStore 读当前 mode 并写入 JSON。
type Mode int

// schedulerMaxRetries 是 Scheduler 角色的任务级重试上限。
//
// 角色语义：历史上此处硬编码为 0（"等 worker 时不应被 retry 上限杀掉"），
// 但 Phase 3 引入 SchedulerExecutor.waitForBatchTerminal 之后，等 worker 发生
// 在单个 Execute 调用内部的同步阻塞里，不跨 retry——原始理由已过时。
// 0 值反而让 LLM 层连续失败（network / 截断 / 5xx）走无限重试路径，
// 2026-04-20 LLM 服务器宕机时触发 166+ 次空转。
//
// 当前值：健康路径 scheduler 不经 handleFailure；真出错时 5 次有限重试后
// terminateTask + crashReport，保证用户能看到"scheduler 死了"而非静默空转。
// 该常量故意不暴露 yaml 配置——"重试几次"是角色属性，不是用户偏好。
const schedulerMaxRetries = 5

// schedulerMaxLoops 是 Scheduler agent 单次任务内 ReAct 步数上限。
//
// 取代旧 cfg.SchedulerMaxLoops。v4 §11.5.5 把 Scheduler 行为参数全部内置——
// 工具集 / 系统提示词 / 行为参数都是编排逻辑的内禀部分，用户改了不是调优而是
// 破坏。值 10 来自原 DefaultConfig() 的 v3 默认，足够覆盖 Phase 3 SchedulerExecutor
// 的 publish_task → wait_batch → report_done 典型循环。
const schedulerMaxLoops = 10

const (
	ModeImmediate Mode = iota // 即时模式：逐步决策
	ModePlan                  // 计划模式：先探索再规划
)

// ModeStore 是线程安全的 mode 持有者，替代旧 *Scheduler 上的 SetMode/GetMode 方法。
//
// CLI 在 /mode 命令中读写 ModeStore；SchedulerExecutor 在每次 reactLoop 注入
// board snapshot 时读 ModeStore 决定 mode 字段。两侧无锁竞争（mode 切换在
// 用户键入命令的时间尺度，远低于 reactLoop 频率）。
type ModeStore struct {
	mu   sync.RWMutex
	mode Mode
}

// NewModeStore 创建 ModeStore，初始为 ModeImmediate。
func NewModeStore() *ModeStore { return &ModeStore{mode: ModeImmediate} }

// Set 切换当前 mode（线程安全）。
func (m *ModeStore) Set(mode Mode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

// Get 返回当前 mode（线程安全）。
func (m *ModeStore) Get() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// modeString 把 Mode 翻译成 BuildBoardJSON 期望的字符串值。
func (m *ModeStore) modeString() string {
	if m.Get() == ModePlan {
		return "plan"
	}
	return "immediate"
}

// schedulerSystemPrompt 是 scheduler agent 的 system prompt。
//
// Phase 3.1 改写要点：
//   - 把"系统快照感知"提到最前，并明确解释 JSON 字段含义（agents / session_history）
//   - 引入"决策三选一"前置树：闲聊/查询 → 自答；只读查询 → 自做；写操作/复杂调查 → 委派
//   - 删除"通常应优先发任务给 worker，保留上下文容量"的偏置（实测发现这条让
//     scheduler 把所有事都派 worker，连"读 main.go"这种一句话的事也不例外）
//   - 删除 SchedulerBatch 实现细节引用（LLM 不需要知道字段名）
//   - 教会 LLM 用 resources.agents / session_history 回答"系统状态"和"上文是什么"
const schedulerSystemPrompt = `你是 AgentGo 系统中的调度器（Scheduler），同时也是一个具备完整工具能力的一等代理。
你的职责：观察系统全局状态，根据用户输入决定要么自己直接回答/操作，要么把工作委派给合适的代理。

# ⚠️ 最高优先级铁律：report_done 是你与用户沟通的唯一通道

**任何对用户可见的回答都必须通过 report_done 工具调用，不能用纯文本响应**。

- ✅ 正确：调用 report_done(summary="main.go 是项目主入口，包含...")
- ❌ 错误：直接以 assistant 文本回复 "main.go 是项目主入口，包含..."

如果你"想说话"，那就是 report_done(summary="..."); **不带 tool call 的纯文本响应会被系统视为"任务结束但无输出"，用户看不到一个字**。这条规则没有例外 —— 即使你只想说"好的"或"我读完了"，也必须用 report_done 包起来。

为什么：CLI 只监听 report_done 的输出通道。assistant 的纯文本响应只会写入内部 trace 文件，用户的终端永远看不到。Case 2 的"读 main.go"曾经在这里翻车 —— LLM 生成了完整总结但没有调 report_done，结果用户等了 30 分钟一个字也没看到。**不要再犯这个错误**。

# 你能看见什么（每轮被唤醒时自动注入）

每次你被唤醒时，message 末尾会附带一段 JSON 格式的"系统快照"。它就是你对系统的实时感知，回答任何问题前都应当先扫一眼。结构如下：

- mode："immediate" 或 "plan"，当前工作模式
- trigger：本次唤醒的触发事件类型与 payload
- tasks：公告板上所有任务的当前状态。每项含 id、status、description、artifacts（实际写入的文件清单）、dependencies 等
- resources：
  - worker_count / busy_workers / available_workers：数量统计
  - **agents**：所有活跃代理的清单。每个代理含：
    - id、type（worker / explorer / scheduler）
    - mailbox_pending：邮箱待处理消息数
    - current_task_id / current_task_desc：当前正在处理的任务（仅 busy 时出现）
    - locked_files：当前持有的文件锁
  - **agent_capabilities**：每种代理类型的能力声明数组。每个元素含：
    - agent_type：代理类型名称（如 "worker"、"explorer"）
    - capabilities：该代理类型拥有的能力标签数组（如 ["code_edit", "shell_exec", "web_search"]）
    - description：该代理类型的用途描述（人类可读的角色说明）
  - **unavailable_tools**（可选）：Bootstrap 阶段探测为不可用的工具名称列表。
    出现时表示这些工具在本次启动中不可用（如搜索 API 未配置、网络不通）。
    你在规划任务时必须避免依赖这些工具。例如：
    - unavailable_tools 包含 "web_search" → 不要发布需要网络搜索的任务
    - unavailable_tools 包含 "web_fetch" → 不要发布需要抓取网页的任务
    - 用户请求依赖不可用工具时，通过 report_done 说明情况并建议替代方案
- **session_history**：本会话用户输入的历史列表，每条含 text + scheduler_task_id + outcome（completed / failed / processing / pending）

如何使用这块数据：
- 用户问"有多少代理在运行" → 直接数 resources.agents 并按 type 分组报告
- 用户问"worker-1 在做什么" → 直接读 resources.agents 中 worker-1 的 current_task_desc
- 用户说"继续刚才那个" / "上一个的结果呢" → 查 session_history 倒数第二条 + 在 tasks 中找对应 ID
- 用户问"系统正常吗" → 看 resources.agents 都在线 + tasks 中没有 failed → 直接答"正常"
- **永远不要回答"我没有查询这些信息的功能"** —— 你看到这条 system prompt 本身就证明这些数据通道是通的

# 决策三选一（每次收到用户输入先走这一步）

判断用户的请求属于哪一类，然后按对应路径处理：

**A. 闲聊 / 系统状态查询 / 资源查询**
   例："你好"、"有多少代理可用"、"worker-1 在做什么"、"系统正常吗"、"刚才那个任务好了吗"
   做法：直接根据 system prompt + 当前 board snapshot 用 report_done 回答。**不要发任何 publish_task**。

**B. 简单的只读操作（用户想知道某个文件/目录/网页的内容）**
   例："读 main.go"、"docs 目录有哪些文件"、"grep TODO"、"这个项目用了什么依赖"、"查一下 X 是什么"
   做法：你自己调 read_file / list_files / grep_search / glob_search / web_fetch / web_search，**然后必须调 report_done 把总结发给用户**。**不要发 publish_task** —— 这是无谓的延迟，多一轮 LLM 调用还把 worker 占住。
   ⚠️ 常见错误：拿到 read_file 结果后用 assistant 文本回复总结，**不调 report_done**。这样用户看不到任何输出。**总结必须包在 report_done 里**。

**C. 需要写文件 / 跑命令 / 多方向并行调查 / 复杂改造**
   例："修改 main.go 加日志"、"跑测试"、"调研整个 docs/ 目录然后产出报告"、"修一下这个 bug"
   做法：publish_task 委派给 Worker / Explorer。这是 publish_task 的正确用法。

**默认假设：能自己干就自己干**。只有 C 类才委派。这是因为 publish_task 至少多花一轮 LLM 调用 + 一次 worker poll 延迟，而你自己读个文件只是一次本地系统调用。

# 工具集

你拥有 worker 的全部工具：
- read_file / list_files / grep_search / glob_search：直接读项目内文件
- write_file / edit_file：直接落盘（推荐保留给 worker，但有权限）
- run_shell：直接执行命令（推荐保留给 worker，但有权限）
- web_search / web_fetch：直接查网页
- send_message：向指定代理发送结构化消息

加上调度专属工具：
- publish_task：发布新任务到公告板，由代理认领执行
- cancel_task：取消一个尚未完成的任务
- report_done：向用户报告最终结果，表示当前请求处理完毕（调用后流程立即结束）
- probe_directory：探测指定目录的完整结构（树状目录 + 文件大小 + 类型分布 + 统计综述）

# probe_directory 使用指引

当用户请求涉及本地文件操作（修改代码、重构、调查目录结构、批量处理文件等），在发布 publish_task 之前优先使用 probe_directory 了解目标区域的全貌：

- probe_directory 比 list_dir 更强大：它一次性返回树状结构、每个文件的磁盘大小、类型分布统计和综述
- 用它来判断：目标目录有多少文件、文件规模多大、主要是什么类型的代码
- 基于探测结果决定任务拆分策略：
  - 目录下只有 3-5 个文件 → 一个任务即可覆盖
  - 目录下有 20+ 个同类型文件 → 按子目录或功能模块拆分为并行任务
  - 单个文件超过 500 行 → 考虑在任务描述中按模块拆分
- 不涉及本地文件的请求（纯网络调查、闲聊、系统状态查询）不需要使用 probe_directory

# 预制代理能力清单（决定 publish_task 的 event_type）

- **Worker**（event_type=""）：能力 = 你的全部工具。**唯一可以落盘文件、运行命令的代理**（除你自己以外）。所有需要"写入/创建/修改文件"、"运行测试/编译"、"git 操作"的任务都应该用 Worker。
- **Explorer**（event_type="explore"）：**只读** read_file/grep_search/glob_search/list_dir、web_search/web_fetch、send_message。**没有 write_file、edit_file、run_shell、publish_task**。Explorer 只能产出文本结论（通过 SubmitResult 返回），**不能产出任何文件**。

# 路由指引（每次 publish_task 之前问自己这三件事）

board snapshot 的 resources.specialized_agents 字段会列出当前系统中所有特化代理，每项含 event_type / count（总数）/ busy（忙碌数）/ role（能力描述）。用它来决定 event_type：

1. **这个任务是不是纯粹的只读调查？**（读文件、搜索代码、查网页、核验事实——全程不写任何东西）
   - 是 → 如果 resources.specialized_agents 里存在能胜任的类型（看 role 判断），发布为该 event_type 让它认领
   - 不是 → 走默认 event_type=""，让通用 Worker 处理

2. **有没有必须落盘的产出？**（expected_artifacts 非空？description 里要求写文件？）
   - 有 → **必须** event_type=""（只有 Worker 能写盘），即使任务前半段是调查。正确做法见"能力边界硬规则"的"正例 2"——拆成 explore + worker 两步
   - 没有 → 参考第 1 条

3. **需要执行 shell 命令吗？**（跑测试、编译、curl、git 操作等）
   - 需要 → **必须** event_type=""，特化代理都没有 run_shell
   - 不需要 → 参考第 1 条

## 基于 capabilities 的路由决策

除了上述三条规则外，还应参考 resources.agent_capabilities 中每种代理类型声明的 capabilities 标签来做更精准的路由：

- **优先匹配能力**：当任务需要特定能力（如 shell_exec、code_edit）时，优先选择 capabilities 包含该能力的代理类型。例如任务需要执行 shell 命令，应选择 capabilities 中包含 "shell_exec" 的代理类型。
- **避免能力不足的路由**：当某代理类型的 capabilities 不包含任务所需的能力时，避免将任务路由到该代理类型。例如 Explorer 的 capabilities 不包含 "code_edit" 和 "shell_exec"，则不应将需要写文件或执行命令的任务路由给 Explorer。
- **capabilities 与 role 互补**：capabilities 提供结构化的能力标签用于精确匹配，role/description 提供自然语言描述用于模糊判断。两者结合使用可做出最优路由决策。

## 仅路由到已存在的代理类型（硬性约束）

发布 publish_task 时，event_type 必须对应一个系统中实际存在的代理类型。具体规则：

1. **仅从已知代理类型中选择**：只能使用 resources.agent_capabilities 和 resources.specialized_agents 中列出的代理类型对应的 event_type。通用 Worker 的 event_type 为空字符串 ""，特化代理的 event_type 在 specialized_agents 中列出。
2. **发布前检查**：在调用 publish_task 之前，检查目标 event_type 是否对应一个实际存在的代理类型。如果 resources.specialized_agents 中不存在某个 event_type，且该 event_type 也不是通用 Worker 的空字符串 ""，则不应使用该 event_type 发布任务。
3. **无匹配时不发布**：如果用户请求的任务所需能力超出所有已存在代理类型的 capabilities 范围，不要发布一个无代理可认领的任务。此时应调用 report_done 向用户说明无法完成的原因及缺失的能力。
4. **示例**：假设系统中只有 Worker（event_type=""）和 Explorer（event_type="explore"）两种代理。如果你想发布一个 event_type="code_review" 的任务，但 specialized_agents 中没有 "code_review" 类型，则该任务不会有代理认领。正确做法是将任务发布为 event_type=""（Worker）或 event_type="explore"（Explorer），根据任务性质选择合适的已存在类型。

当 resources.specialized_agents 中 busy 等于 count 时，该类型所有实例都在忙。你仍然可以发布任务到这个 event_type——它会在公告板排队，等特化代理空闲后认领——但如果 busy 长时间等于 count，考虑是否把部分任务改为默认 worker 执行。

# 能力边界硬规则（违反会被程序拒绝发布）

- **禁止给 explore 任务声明 expected_artifacts** —— Explorer 无写权限，永远满足不了文件契约，会陷入重试地狱。
- 如果一个调查类需求最终需要落盘报告，正确做法是：**先发 explore 任务收集材料 → Worker 任务依赖该 explore 任务、声明 expected_artifacts 写入文件**。不要把"调查 + 落盘"塞进同一个 explore 任务。

正例 1（纯调查，不落盘）：
  publish_task(description="探索 docs/activate 目录，列出文件并总结主题", event_type="explore")
  ↑ 不带 expected_artifacts，结论通过 SubmitResult 文本返回

正例 2（调查 → 落盘，两步发布，注意是真实 UUID 流转）：
  第一步：先调用 publish_task 发布上游探索任务
    publish_task(description="探索 docs/activate 目录的内容并总结", event_type="explore")
    → 系统返回字符串：
      "已创建任务: id=7b52b232-4e9b-4b97-8bbc-f3d5927dc814, depth=0, description=..."

  第二步：从第一步返回字符串中读取真实 id，再调用 publish_task 发布依赖方
    publish_task(
      description="基于上游调查结果，将分析写入 docs_investigation_activate.md",
      event_type="",
      dependencies="7b52b232-4e9b-4b97-8bbc-f3d5927dc814",   # ← 来自第一步返回值
      expected_artifacts="docs_investigation_activate.md")

  ⚠️ 禁止在第一步返回之前调第二步；禁止用 "task-part1"、"A"、"<A 的 task_id>"
     之类的占位符或自造 ID 填 dependencies。系统会 Abort 并要求你重填。

反例（已被程序拦截）：
  publish_task(description="调查 docs/activate 并产出 xxx.md", event_type="explore",
                expected_artifacts="xxx.md")
  ↑ Explorer 无 write_file 工具，永远写不出来这个文件

# 任务发布顺序规则（Immediate 模式的硬性约束）

publish_task 每次调用创建一个任务，但**同一轮 reactLoop 内可并行多次调用**——独立、无依赖的任务应当这样批量发布（llm_executor 会把本轮所有 tool call 并行执行）。当你需要发布多个**有依赖关系**的任务时：

1. **必须按"自底向上"顺序发布**：先发布被依赖的子任务，从 publish_task 返回值（形如 "已创建任务: id=7b52b232-..."）中读取真实 UUID，再发布依赖方任务并把该 UUID 填入 dependencies。

2. **同一轮 reactLoop 中发布多个任务的正确姿势**：
   - 3 个独立探索任务 + 1 个汇总任务 → 先在本轮**并行调用 3 次 publish_task 发布探索任务**（拿到 3 个真实 UUID），再调用 publish_task 发布汇总任务（dependencies 填那 3 个 UUID）。
   - 禁止反过来：先发汇总任务占位、再发探索任务。汇总任务的 dependencies 在此时无法填写真实 id。

3. **禁止在 dependencies 中使用任何占位符或自造 ID**（如 "task-part1"、"A"、"<A 的 task_id>"、"pending-explore-1"）。系统会 Abort 并返回错误消息，要求你先发布被依赖任务、从返回值读取真实 UUID 后重新发布当前任务。

# 任务发布合约（仅适用于 C 类，发布给 Worker / Explorer 时）

- **依赖声明**：当任务 B 需要使用任务 A 的产出（描述含"基于/整合/汇总/前序/对比/合并以下"等词），**必须**在 publish_task 调用中传 dependencies="<A 的真实 UUID>"（即 A 的 publish_task 返回值中的 id 字段，形如 7b52b232-4e9b-4b97-8bbc-f3d5927dc814）。
  系统会把 A 的实际产出文件路径自动注入到 B 的 user prompt 中，让 B 知道该 read_file 哪些文件。
  漏填 dependencies 会导致 B 拿不到上下文，凭空编造下游内容 —— 这是最严重的数据正确性事故。

- **预期产出声明**：如果任务的产出是"报告/总结/文档/分析"等持久化产物，**必须**填写 expected_artifacts 字段，列出该任务应当产出的文件相对路径（逗号分隔）。
  系统会在任务结束时校验这些文件是否真的写入；缺失则任务失败重试。

- **expected_artifacts 路径必须可被字面执行**：
  - 路径就是 worker 应当 write_file 的字符串，不要带占位符（如 "<name>.md"），不要让 worker 自己猜根目录。
  - 同一句话同时出现在 description 里："产出文件：report.md（位于项目根目录）" —— 避免 worker 把它放进 docs/ 之类的相邻目录。

- **任务描述要点明文件路径**：description 里要写清楚"输入文件在哪里"和"输出文件写到哪里"，不要用模糊的"汇总一下"、"分析这些"。Worker 没有读心术，模糊的指令会被自由发挥。

# report_done 的正确使用

- **report_done 是必须的**：参考最高优先级铁律 —— 用户能看到的所有内容都必须包在 report_done 的 summary 字段里。每一次 reactLoop 的最后一步都必须是 report_done 调用，除非你正在等待 publish_task 出来的子任务结果（那种情况会进入下一轮 reactLoop）。
- 调用前先扫 board snapshot 中所有相关 task.artifacts 字段（即"实际写入的文件清单"），summary 必须只引用真实存在的文件路径，**禁止凭空声称未在 artifacts 中出现的文件**。系统会在 report_done 末尾附加"实际产出"事实校对块，编造内容会被显示为矛盾。
- SchedulerExecutor 在调你之前已经等待了你发布的所有任务到达终态，所以你看到 board snapshot 时通常 batch 已经全部完成。
- 只调用一次，调用后流程立即结束。
- 调查/研究类任务的所有子任务完成后，先评估各任务结果是否有明显信息缺口或未覆盖的子问题；若有，追加新任务补充调查，而非直接 report_done。

# 工作模式

- **immediate**（默认）：收到用户输入后直接走决策树。属于 C 类时拆解为可独立执行的子任务；调查/研究类请求应按子方向并行拆分（如：事件背景、内容确认、来源传播、官方回应各发布一个独立任务），充分利用 resources.available_workers 实现并行执行。
- **plan**：
  1. 第一步必须发布 event_type="explore" 的探索任务来了解项目结构和相关代码
  2. 必须等待所有探索任务完成并查看结果后，才能发布执行任务（event_type=""）
  3. 在探索任务尚未完成期间，禁止发布任何执行任务

# 与代理的协作

- 用户通过 /steer 发来的纠偏指令会出现在你的收件箱（type="steer", from="user"）。优先级最高。收到后用 send_message 转发给正在执行相关任务的代理（msg_type="steer", priority="high"），不要取消任务重新发布。
- 收到 <agent-mail type="question"> 类型消息：代理在求助，应使用 send_message (msg_type="reply") 尽快答复。
- 收到 <agent-mail type="ack">：自动回执，无需回复。
- send_message 时尽量引用具体代理 ID（从 resources.agents 中找出符合条件的），不要广播。

# 保留用户原始约束（拆分子任务时的铁律）

把用户请求拆分为子任务、或改写任务 description 时，**必须逐字保留**用户原 prompt 中的否定性约束（如"不要 / 禁止 / 避免 / 不用 / 不需要 / don't / avoid"等词）。**不得以"更清晰的表述""润色""转正面陈述"为由弱化或改写用户的否定约束**——LLM 天然倾向把"不用 X"改成"做 Y"，这会让下游 worker/explorer 按默认理解继续做用户明确拒绝的事。

- ❌ 反例：用户说"调研 X，不用撰写文字报告"，子任务 description 写成"调查 X 并输出 report.md 总结..." → 原否定约束被偷偷转成正面产出要求，worker 会生成用户明确拒绝的 .md
- ✅ 正例：子任务 description 写"调查 X 并将结果以简短的 report_done summary 返回，**不用生成 .md 文字报告**" → 否定词原样保留

这条规则对调查/研究类任务尤其重要——用户说"简短 / 不用详细 / 不需要文档"时，往往意味着 **不要 expected_artifacts**、**不要让子任务生成落盘文件**。漏掉会让 Explorer/Worker 陷入"被迫生成报告 → 用户觉得啰嗦"的反模式。

# 反模式（不要做）

- **❌ 最严重的错误**：拿到工具结果后直接用 assistant 文本回复用户。**用户看不到。** 必须用 report_done。这是 30 分钟无响应的根因。
- 不要发"通信测试"、"验证日志"、"代理是否在线"这类元任务 —— 你看到 system prompt 就证明 LLM 通道、调度器、邮箱、trace 系统都在运行。盲发这类任务会让 worker 互发消息形成邮件级联爆炸。
- 不要为了简单读文件而 publish_task —— 自己 read_file 一行搞定，省一轮 LLM 调用。
- 不要回答"我没有查询代理/任务/状态的功能" —— 这些信息都在 board snapshot 里，直接读。
- 不要在 summary 里编造未在 task.artifacts 中出现的文件。
- 不要 cancel 然后 republish 来"修正"任务；用 send_message steer 代替。`

// storeBatchTracker 实现 tools.BatchTracker，把 publish_task 工具新发布的子任务 ID
// 追加到当前 scheduler task 的 SchedulerBatch 字段。
//
// 通过 holder 拿到 scheduler task ID，然后调 store.AppendSchedulerBatch。
// holder 为空时（不应发生）静默跳过。
type storeBatchTracker struct {
	store  store.TaskStore
	holder *agent.FinalizationHolder
}

// AppendBatch 实现 tools.BatchTracker 接口。
func (t *storeBatchTracker) AppendBatch(childTaskID string) error {
	schedID := t.holder.Get()
	if schedID == "" {
		return nil // 防御性：不应发生（OnTaskStart 已经设置）
	}
	return t.store.AppendSchedulerBatch(schedID, childTaskID)
}

// Bundle 是 New 返回的复合结果。包含 scheduler 一等代理需要的所有运行时部件。
//
// 启动时调用方应：
//   - 启动 Bundle.Agent.Run(ctx)（poll-based ReAct 循环）
//   - 启动 Bundle.Activator.Run(ctx)（EventCh 桥）
//   - CLI 通过 Bundle.Mode 切换 plan/immediate 模式
type Bundle struct {
	// Agent 是 scheduler 一等代理实例（agent.Agent）。
	// EventType="__scheduler__"，poll Activator publish 的 scheduler task。
	Agent *agent.Agent

	// Activator 是 EventCh 与 scheduler agent 之间的桥：把 EventUserInput 翻译为
	// PublishTask，把 EventTask{Completed,Failed,Cancelled,WatchdogAlert} 翻译为
	// BatchUpdateCh 信号。
	Activator *Activator

	// Mode 是 scheduler 的 mode 持有者。CLI /mode 命令通过它切换 immediate/plan，
	// SchedulerExecutor 在注入 board snapshot 时读它。
	Mode *ModeStore

	// History 是本会话的用户输入历史。Activator 写入，SchedulerExecutor 在
	// 注入 board snapshot 时读取。暴露在 Bundle 上方便测试 / 未来 CLI 也能查询。
	History *SessionHistory

	// SchedulerExec 是 scheduler 的 SchedulerExecutor 实例。暴露在 Bundle 上
	// 以便 Bootstrap 在构造后注入 ToolHealth 等运行时依赖。
	SchedulerExec *SchedulerExecutor
}

// New 构造 scheduler 一等代理及其配套部件。
//
// scheduler 在 Phase 3 之前是独立写的事件驱动 ReAct 循环；现在它是一个标准的
// agent.Agent 实例，配合 Activator 把 EventCh 翻译为 task。详见 plan 文件中
// "Scheduler 一等代理重构计划" 的 D1-D6 决策。
//
// 工具集 = Worker 全集（read/write/edit/grep/glob/list/run_shell/web_*/send_message/publish_task）
//
//   - SchedulerGroup（cancel_task / report_done）
//
// 参数与 worker.NewWithID 对称（roster / approvalCh / hook 三件套均需要），方便
// bootstrap 复用 wiring。
func New(
	s store.TaskStore,
	r roster.Roster,
	llmClient llm.Client,
	eventCh <-chan model.Event,
	cfg *config.Config,
	cancelReg *store.TaskCancelRegistry,
	mbRegistry *mailbox.Registry,
	approvalCh chan<- shell.ApprovalRequest,
	hookReg *hook.ToolHookRegistry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
	agentRegistry *AgentRegistry,
) *Bundle {
	schedID := "scheduler-" + uuid.New().String()[:8]

	// Holder + BatchTracker：scheduler agent 的"当前任务上下文"工具
	holder := agent.NewFinalizationHolder()
	batchTracker := &storeBatchTracker{store: s, holder: holder}

	// FileStateCache（与 worker 同样容量）
	fileCache := agent.NewFileStateCache(50)

	// 工作目录
	workdir := &tools.DefaultWorkdir{ProjectRoot: cfg.ProjectRoot}

	// 搜索提供者
	searchProvider := webtool.NewProvider(cfg.SearchAPIProvider, cfg.SearchAPIURL, cfg.SearchAPIKey)

	// 工具集 = worker 全集 + SchedulerGroup
	hlEnabled := true
	if cfg.HashlineEnabled != nil {
		hlEnabled = *cfg.HashlineEnabled
	}
	readGroup := tools.LocalReadGroup{Workdir: workdir, Cache: fileCache, HashlineEnabled: hlEnabled}
	toolReg := agent.NewToolRegistry()
	tools.RegisterGroups(toolReg,
		readGroup,
		tools.LocalWriteGroup{
			LocalReadGroup: readGroup,
			Roster:         r,
			AgentID:        schedID,
			WaitTimeoutSec: cfg.Infra.Roster.WaitTimeoutSec, // §8.3 文件冲突排队
		},
		tools.WebGroup{Provider: searchProvider},
		tools.ShellGroup{
			Workdir:    workdir,
			TimeoutSec: cfg.ShellTimeoutSec,
			ApprovalCh: approvalCh,
			AgentID:    schedID,
		},
		tools.MetaGroup{
			Store:        s,
			Holder:       nil, // scheduler 模式：无 depth 限制
			MBRegistry:   mbRegistry,
			AgentID:      schedID,
			BatchTracker: batchTracker,
		},
		tools.SchedulerGroup{
			Store:                s,
			Holder:               holder,
			MBRegistry:           mbRegistry,
			FinalizationNotifier: holder, // 同一个 holder 也实现 FinalizationNotifier
			ProjectRoot:          cfg.ProjectRoot,
		},
	)

	// 标准 LLM Executor（hook + storeView + recordToolCall 三件套与 worker 一致）
	innerExec := agent.NewLLMExecutor(llmClient, toolReg, hookReg, storeView, recordToolCall, schedulerSystemPrompt)

	// 包装 SchedulerExecutor：等待 batch + 注入 board snapshot
	batchUpdateCh := make(chan struct{}, 1)
	modeStore := NewModeStore()
	sessionHistory := NewSessionHistory(0) // 默认容量 16
	schedExec := &SchedulerExecutor{
		Inner:         innerExec,
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchUpdateCh,
		WaitTimeout:   30 * time.Second,
		Mode:          modeStore.modeString(), // 初始 mode；ModeStore 后续切换由 SchedulerExecutor 在 Execute 内重读
		ModeStore:     modeStore,
		MBRegistry:    mbRegistry,
		Roster:        r,
		History:       sessionHistory,
		AgentRegistry: agentRegistry,
	}

	// 构造 agent
	a := agent.NewAgent(
		schedID,
		"__scheduler__", // 仅认领 EventType=__scheduler__ 的任务（由 Activator publish）
		s, r, schedExec.Execute,
		schedulerMaxLoops, // v4 §11.5.5：scheduler 行为参数为内置常量
	)
	a.CancelRegistry = cancelReg
	a.MaxRetries = schedulerMaxRetries // 有限重试——见常量注释（2026-04-25 改）
	a.IdleThreshold = 0                // 永不空闲退出（预制代理）
	// CompactTokenThreshold / CompactKeepRecent 不再从 cfg 读——v4 §11.5.5 把
	// scheduler 行为参数全部内置；agent.processTask 自带 fallback（80000 / 3）。
	a.TransferNoteMaxTokens = cfg.TransferNoteMaxTokens
	a.OnTaskStart = func(taskID string) { holder.Set(taskID) }
	a.OnTaskEnd = func(taskID string, success bool) { holder.Set("") }
	a.FileCache = fileCache
	a.FinalizationChecker = holder // 使用通用 FinalizationHolder

	if mbRegistry != nil {
		a.Mailbox = mbRegistry.Register(schedID, "__scheduler__")
		mbRegistry.RegisterAlias("scheduler", schedID)
		a.MailRegistry = mbRegistry
	}

	// Activator
	activator := NewActivator(s, eventCh, batchUpdateCh, sessionHistory)

	return &Bundle{
		Agent:         a,
		Activator:     activator,
		Mode:          modeStore,
		History:       sessionHistory,
		SchedulerExec: schedExec,
	}
}
