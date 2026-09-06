package session

import (
	"agentgo/internal/contextruntime"
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const turnLedgerFile = "turns.jsonl"

// AppendModelOutput 把完成轮次追加到指定 Session 的 turns.jsonl。sessionID 必须是
// 完整 ID，不能用前缀；这样即使运行时同时发生 Session 切换，轮次仍落到
// 事件产生时绑定的观测边界。每轮只做一次 write + fsync，不在流式 delta
// 路径写盘。
func (sm *SessionManager) AppendModelOutput(turn contextruntime.OutputRecord) error {
	sessionID := turn.Identity.SessionID
	if turn.Schema != "agentgo.model-output/v1" {
		return fmt.Errorf("拒绝模型输出契约")
	}
	if sm == nil {
		return fmt.Errorf("SessionManager 为空")
	}
	if turn.Identity.InvocationID == "" {
		return fmt.Errorf("轮次记录缺少 id/agent_id")
	}
	if turn.Status != "completed" && turn.Status != "failed" {
		return fmt.Errorf("轮次记录状态 %q 无效", turn.Status)
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	path, err := sm.turnLedgerPathLocked(sessionID)
	if err != nil {
		return err
	}
	turn.Identity.SessionID = sessionID
	return AppendModelOutputFile(path, turn)
}

// AppendModelOutputFile 是业务会话与独立操作共用的 L3 轮次写入，不提供旧格式转换。
func AppendModelOutputFile(path string, turn contextruntime.OutputRecord) error {
	if turn.Schema != "agentgo.model-output/v1" || turn.Identity.InvocationID == "" || (turn.Identity.SessionID == "" && turn.Identity.OperationID == "") {
		return fmt.Errorf("模型输出身份或版本无效")
	}
	if turn.Status != "completed" && turn.Status != "failed" {
		return fmt.Errorf("模型输出尚未结束")
	}
	if turn.Status == "completed" && (turn.Result == nil || turn.Result.Data().Schema != "agentgo.model-result/v1") {
		return fmt.Errorf("完成的模型输出缺少完整结果")
	}
	data, err := json.Marshal(turn)
	if err != nil {
		return fmt.Errorf("序列化轮次记录失败: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开轮次账本失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入轮次账本失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("同步轮次账本失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭轮次账本失败: %w", err)
	}
	return nil
}

// LoadModelOutputs 按追加顺序读取当前格式；旧格式与损坏记录明确拒绝。
func (sm *SessionManager) LoadModelOutputs(sessionID string) ([]contextruntime.OutputRecord, error) {
	if sm == nil {
		return nil, fmt.Errorf("SessionManager 为空")
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	path, err := sm.turnLedgerPathLocked(sessionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开轮次账本失败: %w", err)
	}
	defer f.Close()

	var turns []contextruntime.OutputRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var turn contextruntime.OutputRecord
		if err := json.Unmarshal(scanner.Bytes(), &turn); err != nil {
			return nil, fmt.Errorf("模型输出账本第 %d 行损坏: %w", lineNum, err)
		}
		if turn.Schema != "agentgo.model-output/v1" || turn.Identity.InvocationID == "" || turn.Identity.SessionID != sessionID {
			return nil, fmt.Errorf("拒绝旧版本或跨会话模型输出")
		}
		turns = append(turns, turn)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("扫描轮次账本失败: %w", err)
	}
	return turns, nil
}

func (sm *SessionManager) turnLedgerPathLocked(sessionID string) (string, error) {
	if sessionID == "" || sessionID != filepath.Base(sessionID) {
		return "", fmt.Errorf("无效 Session ID %q", sessionID)
	}
	dir := filepath.Join(sm.baseDir, "sess-"+sessionID)
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("Session %q 不存在: %w", sessionID, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Session %q 不是目录", sessionID)
	}
	return filepath.Join(dir, turnLedgerFile), nil
}
