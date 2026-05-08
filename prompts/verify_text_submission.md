请验证 gatherer 任务 `${gatherer_task_id}`（执行者 `${gatherer_agent}`）的整合报告。

# 报告在哪里

这次事件是 `text_only_submission`——gatherer 退出时**没有写文件**（`output_len=${output_len}`，`loops=${loops_used}`）。报告内容通过依赖机制注入到本任务的 user prompt 顶部，形如：

```
--- 前置任务结果 ---
[task_id=${gatherer_task_id}] <gatherer 的整合 Markdown>
```

如果该 dep 段尾部出现「该任务实际写入的文件」清单（极少见——gatherer 本应纯文字，但若历史任务残留或同 ID 复用产生了文件，会出现在这里），请优先 `read_file` 那些路径作为审核对象；否则**直接以 dep 段里的 Markdown 文字为审核对象**。

# 操作步骤

1. **读取审核对象**：
   - 优先：dep 段尾部如有具体文件路径列表 → `read_file` 那些精确路径（不要 `list_dir reports/`）
   - 否则：审核对象就是 dep 段里 `[task_id=...]` 后面的那段 Markdown 文字
2. **覆盖面打分**：对照系统提示中的"覆盖面 Rubric"给 6 维度 0/1/2 评分
3. **决策**：
   - **合格**（总分 ≥ 9/12 且无 0 分维度）→ `write_file` 写印记：
     - 若审核对象是文件，印记路径为 `<原文件名>_APPROVED.md`
     - 若审核对象是 dep 文字，印记路径为 `reports/text_only_${gatherer_task_id}_APPROVED.md`
     - 印记内容按系统提示规定的固定结构；"原报告"字段写明文件路径或 "text-only submission, gatherer task=${gatherer_task_id}"
     - 最后输出文字摘要，包含印记路径和 gatherer task ID
   - **不合格** → `publish_task` 派一个聚焦补充任务回默认队列，描述以"针对缺漏面 rework"开头，注明：原 gatherer task ID（`${gatherer_task_id}`）、缺漏维度、具体期望。最后输出文字摘要：未通过原因 + 已派任务 ID

# 关键纪律（防级联爆炸）

- **不要 `send_message` 给 gatherer 询问任何事**——所需信息都在描述或 dep 段里；问询会唤醒 gatherer 引发循环
- 同名 `_APPROVED.md` 已存在（并行 verify 任务已审过）：直接以"已批准过"为理由完成任务
- dep 段为空（理论不应发生）：以"上游产出不可读"为理由完成任务，**不发任何消息**
