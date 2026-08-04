// Package modes 实现 AgentGo 的两轴工作模式体系。
//
// 两个模式轴相互正交、可任意组合：
//   - ExecMode：执行权限轴（normal / strict / readonly / yolo）——执行权限档位；
//   - TopoMode：编排拓扑轴（team / solo）——多代理团队协作或单代理编排。
//
// （V6 C6c 前曾有第三轴 gate（规划门控）；其 plan 值已于 V6 移除、执行前审阅
// 改由 Graph approval 节点承担，轴实体在 C6c 整体删除。）
//
// 本包只承载模式值的定义、解析与线程安全持有（Store），不实现任何行为语义；
// 行为由各消费方（scheduler 快照注入、后续强制执行切片）按轴读取后自行落地。
package modes

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ============================================================
// 执行权限轴（exec）
// ============================================================

// ExecMode 执行权限轴：声明系统当前的执行权限档位。
// 本切片只承载声明与传递；各档位的行为约束由后续切片落地。
type ExecMode int

const (
	ExecNormal   ExecMode = iota // 正常执行权限
	ExecStrict                   // 严格模式
	ExecReadonly                 // 只读模式
	ExecYolo                     // 放权模式
)

// String 返回小写 snake 字符串（"normal" / "strict" / "readonly" / "yolo"）。
func (e ExecMode) String() string {
	switch e {
	case ExecStrict:
		return "strict"
	case ExecReadonly:
		return "readonly"
	case ExecYolo:
		return "yolo"
	default:
		return "normal"
	}
}

// ParseExecMode 解析 exec 轴字符串（容错大小写与首尾空白），未知值报错。
func ParseExecMode(s string) (ExecMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "normal":
		return ExecNormal, nil
	case "strict":
		return ExecStrict, nil
	case "readonly":
		return ExecReadonly, nil
	case "yolo":
		return ExecYolo, nil
	default:
		return ExecNormal, fmt.Errorf("未知执行权限模式 %q（可选值: normal / strict / readonly / yolo）", s)
	}
}

// ============================================================
// 编排拓扑轴（topo）
// ============================================================

// TopoMode 编排拓扑轴：声明系统是多代理团队协作还是单代理编排。
// 本切片只承载声明与传递；拓扑行为约束由后续切片落地。
type TopoMode int

const (
	TopoTeam TopoMode = iota // 团队编排
	TopoSolo                 // 单代理编排
)

// String 返回小写 snake 字符串（"team" / "solo"）。
func (t TopoMode) String() string {
	if t == TopoSolo {
		return "solo"
	}
	return "team"
}

// ParseTopoMode 解析 topo 轴字符串（容错大小写与首尾空白），未知值报错。
func ParseTopoMode(s string) (TopoMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "team":
		return TopoTeam, nil
	case "solo":
		return TopoSolo, nil
	default:
		return TopoTeam, fmt.Errorf("未知编排拓扑模式 %q（可选值: team / solo）", s)
	}
}

// ============================================================
// Store：两轴的线程安全持有者
// ============================================================

// Snapshot 是两轴模式的字符串瞬时值（均为小写 snake 字符串），
// 供 board snapshot JSON 注入与 UI 展示使用。
type Snapshot struct {
	Exec string
	Topo string
}

// ErrSnapshotChanged 表示调用方绑定的两轴快照已不再是当前值。
// 它用于把“校验快照 + 应用依赖该快照的 effect”收敛在同一个
// 读锁窗口内，防止校验后、effect 提交前的 TOCTOU 模式漂移。
var ErrSnapshotChanged = errors.New("mode snapshot changed")

// Store 是线程安全的两轴模式持有者，替代旧 scheduler.ModeStore。
//
// 两轴各自独立 Get/Set——一次 Set 只影响本轴，exec 与 topo 的组合
// （如 solo+strict）是合法的并行关系而非互斥枚举。
// 锁竞争假设与旧 ModeStore 一致：模式切换发生在用户键入命令的时间尺度，
// 远低于 reactLoop 频率。
type Store struct {
	// effectMu 把两轴 setter 与 WithSnapshot 的领域事务串行化。
	// 它与纯读 Get/Snapshot 解耦，避免在领域 effect 内触发同步
	// trace/reactor 读取模式时形成 RWMutex 递归读锁风险。
	effectMu sync.Mutex
	mu       sync.RWMutex
	exec     ExecMode
	topo     TopoMode
}

// NewStore 以给定的两轴初值创建 Store。
func NewStore(exec ExecMode, topo TopoMode) *Store {
	return &Store{exec: exec, topo: topo}
}

// DefaultStore 返回两轴全默认（normal / team）的 Store。
func DefaultStore() *Store {
	return NewStore(ExecNormal, TopoTeam)
}

// GetExec 返回当前 exec 轴（线程安全）。
func (s *Store) GetExec() ExecMode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exec
}

// SetExec 切换 exec 轴（线程安全），不影响 topo 轴。
func (s *Store) SetExec(e ExecMode) {
	s.effectMu.Lock()
	defer s.effectMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec = e
}

// GetTopo 返回当前 topo 轴（线程安全）。
func (s *Store) GetTopo() TopoMode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.topo
}

// SetTopo 切换 topo 轴（线程安全），不影响 exec 轴。
func (s *Store) SetTopo(t TopoMode) {
	s.effectMu.Lock()
	defer s.effectMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.topo = t
}

// Snapshot 在一次读锁内取两轴字符串快照，保证两轴值彼此一致（同一次读取）。
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

// WithSnapshot 仅在当前两轴与 expected 完全一致时运行 fn。
// fn 执行期间持有领域 effect 串行锁，因此 SetExec / SetTopo
// 会等待 fn 结束；纯读 Get/Snapshot 仍可安全进行。
// fn 不得回调同一 Store 的 Set/WithSnapshot，否则会重入 effectMu。
func (s *Store) WithSnapshot(expected Snapshot, fn func() error) error {
	if s == nil {
		return fmt.Errorf("%w: mode store is nil", ErrSnapshotChanged)
	}
	if fn == nil {
		return fmt.Errorf("modes: WithSnapshot callback is nil")
	}
	s.effectMu.Lock()
	defer s.effectMu.Unlock()
	actual := s.Snapshot()
	if actual != expected {
		return fmt.Errorf("%w: expected=%s/%s actual=%s/%s",
			ErrSnapshotChanged,
			expected.Exec, expected.Topo,
			actual.Exec, actual.Topo)
	}
	return fn()
}

// snapshotLocked 要求调用方已持有 mu 的读锁或写锁。
func (s *Store) snapshotLocked() Snapshot {
	return Snapshot{
		Exec: s.exec.String(),
		Topo: s.topo.String(),
	}
}
