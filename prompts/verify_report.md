请验证 gatherer 任务 `${gatherer_task_id}`（执行者 `${gatherer_agent}`）刚刚写入的报告文件：

**报告路径：${report_path}**

操作步骤：
1. `read_file` 读取上面那个**精确路径**——不要 `list_dir` 猜文件，不要尝试其它路径
2. 对照系统提示中的"覆盖面 Rubric"打分，给出 6 个维度的 0/1/2 评分
3. 决策：
   - **合格**（总分 ≥ 9/12 且无 0 分维度）→ `write_file` 写一份印记文件，路径为：把原文件名后缀 `.md` 替换为 `_APPROVED.md`（例：`.agentgo/reports/foo.md` → `.agentgo/reports/foo_APPROVED.md`）。印记内容按系统提示规定的固定结构。最后输出文字摘要，必须包含原报告路径与印记路径
   - **不合格** → `publish_task` 派一个聚焦补充任务回默认队列，描述以"针对缺漏面 rework"开头并包含原报告路径、缺漏维度、具体期望。最后输出文字摘要：未通过原因 + 已派任务 ID

**关键纪律（防止级联爆炸）：**

- **绝对不要 `send_message` 给 gatherer 询问任何事**——所有需要的信息（报告路径、gatherer 任务 ID、执行者 ID）都已经在上面给出
- **不要 `list_dir .agentgo/reports/`**——它会让你看到旧报告并产生干扰
- 如果上面的报告路径不可读（read_file 报错），直接以"文件不可读"为理由完成任务，不发任何消息
- 如果原报告已存在 `_APPROVED.md` 印记（另一个 verify 任务已经审过），直接以"已批准过"为理由完成任务
