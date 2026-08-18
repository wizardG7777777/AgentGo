package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const turnLedgerFile = "turns.jsonl"

// TurnRecord 是一次 LLM 调用用户可见输出的 Session 级不可变记录。
// Text 保存 assistant 正文；Reasoning 保存 provider 返回的原始明文思维链。
// 工具参数和工具结果仍不复制到轮次账本。
type TurnRecord struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	AgentID     string    `json:"agent_id"`
	TaskID      string    `json:"task_id"`
	Loop        int       `json:"loop"`
	Text        string    `json:"text"`
	Reasoning   string    `json:"reasoning,omitempty"`
	Status      string    `json:"status"` // completed | failed
	ToolCalls   []string  `json:"tool_calls,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Error       string    `json:"error,omitempty"`
}

// AppendTurn 把完成轮次追加到指定 Session 的 turns.jsonl。sessionID 必须是
// 完整 ID，不能用前缀；这样即使运行时同时发生 Session 切换，轮次仍落到
// 事件产生时绑定的观测边界。每轮只做一次 write + fsync，不在流式 delta
// 路径写盘。
func (sm *SessionManager) AppendTurn(sessionID string, turn TurnRecord) error {
	if sm == nil {
		return fmt.Errorf("SessionManager 为空")
	}
	if turn.ID == "" || turn.AgentID == "" {
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
	turn.SessionID = sessionID
	data, err := json.Marshal(turn)
	if err != nil {
		return fmt.Errorf("序列化轮次记录失败: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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

// LoadTurns 读取指定 Session 的全部有效轮次，保持文件中的追加顺序。
// 损坏行与崩溃留下的半行会告警并跳过，不阻断其余历史恢复。
func (sm *SessionManager) LoadTurns(sessionID string) ([]TurnRecord, error) {
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

	var turns []TurnRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var turn TurnRecord
		if err := json.Unmarshal(scanner.Bytes(), &turn); err != nil {
			log.Printf("[轮次账本] WARN 第 %d 行解析失败，已跳过: %v", lineNum, err)
			continue
		}
		if turn.ID == "" || turn.AgentID == "" {
			log.Printf("[轮次账本] WARN 第 %d 行缺少 id/agent_id，已跳过", lineNum)
			continue
		}
		if turn.SessionID == "" {
			turn.SessionID = sessionID
		}
		turn.ToolCalls = append([]string(nil), turn.ToolCalls...)
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
