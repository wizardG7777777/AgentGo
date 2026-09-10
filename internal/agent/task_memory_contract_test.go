package agent // task_memory_contract_test.go 覆盖 2026-08-20 SWE-001 预防 2：收口契约由
// 系统从持久化任务事实派生写入 TaskMemory.Constraints（渲染时每轮注入，
// 历史压缩碰不到），模型不可改写。
