package trace

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// DegradedMarkerFileName 是 V6 §7.1 trace 降级标记文件名：Writer 进入降级态
// （连续写失败）时在 trace 目录（session 活跃即其 logs/）落此文件，写恢复后
// 清除。CLI（trace show/list）检测到它时在 header 打 trace_degraded 提示。
const DegradedMarkerFileName = "trace_degraded.marker"

// DegradedMarker 是 trace_degraded.marker 的 JSON 内容。
type DegradedMarker struct {
	FirstFailureTime string `json:"first_failure_time"` // RFC3339，本轮降级态首次失败时间
	Error            string `json:"error"`              // 首次失败错误串
	Count            int    `json:"count"`              // 本轮降级态内连续失败次数（每次失败刷新）
}

// recordFailureLocked 记录一次写失败：累计连续失败计数、刷新 marker 文件；
// 首次失败（进入降级态）时返回待触发的 OnDegraded 回调（调用方须在锁外触发），
// 已处于降级态时返回 nil。必须在持有 w.mu 时调用。
func (w *Writer) recordFailureLocked(err error) (cb func(error)) {
	if !w.degraded {
		w.degraded = true
		w.firstFailAt = time.Now()
		w.firstFailErr = err.Error()
		cb = w.onDegraded
	}
	w.failCount++
	w.writeDegradedMarkerLocked()
	return cb
}

// recordSuccessLocked 在一次写成功后清除降级态：删除 marker 文件、复位计数
// 并记恢复日志。未处于降级态时 no-op。必须在持有 w.mu 时调用。
func (w *Writer) recordSuccessLocked() {
	if !w.degraded {
		return
	}
	count := w.failCount
	w.degraded = false
	w.failCount = 0
	w.firstFailAt = time.Time{}
	w.firstFailErr = ""
	if err := os.Remove(filepath.Join(w.dir, DegradedMarkerFileName)); err != nil && !os.IsNotExist(err) {
		log.Printf("[trace] WARNING: 清除降级标记失败: %v", err)
	}
	log.Printf("[trace] trace 写入已恢复（降级期内连续失败 %d 次），降级标记已清除", count)
}

// writeDegradedMarkerLocked 把当前降级态写入 marker 文件（每次失败刷新，
// Count 随失败次数增长）。marker 本身写失败只记日志——绝不能因降级处理
// 再引出新的失败路径。必须在持有 w.mu 时调用。
func (w *Writer) writeDegradedMarkerLocked() {
	m := DegradedMarker{
		FirstFailureTime: w.firstFailAt.UTC().Format(time.RFC3339),
		Error:            w.firstFailErr,
		Count:            w.failCount,
	}
	data, err := json.Marshal(m)
	if err != nil {
		log.Printf("[trace] WARNING: 序列化降级标记失败: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(w.dir, DegradedMarkerFileName), data, 0644); err != nil {
		log.Printf("[trace] WARNING: 落盘降级标记失败: %v", err)
	}
}

// ReadDegradedMarker 读取 dir 下的 trace_degraded.marker。文件不存在或内容
// 不可解析时返回 nil——供 CLI 在 header 打 trace_degraded 提示，也供测试断言。
func ReadDegradedMarker(dir string) *DegradedMarker {
	data, err := os.ReadFile(filepath.Join(dir, DegradedMarkerFileName))
	if err != nil {
		return nil
	}
	var m DegradedMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}
