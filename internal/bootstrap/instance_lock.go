package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// instanceLockFileName 是项目级单实例锁文件名，位于 <projectRoot>/.agentgo/ 下。
const instanceLockFileName = "agentgo.lock"

// acquireInstanceLock 获取项目级单实例锁（E7）。两个 agentgo 进程共用同一项目
// 目录时会互踩共享状态（session 原子写使用固定 .tmp 名会互相覆盖、trace GC 会
// 修剪对方仍在写的文件），因此同一 .agentgo 目录同一时刻只允许一个活跃实例。
//
// 机制：O_CREATE|O_EXCL 创建 <agentgoDir>/agentgo.lock，内容为 pid/host/启动
// 时间。文件创建成功后立即关闭句柄——锁的载体是"文件存在"而非句柄占用
// （Windows 上不关句柄会挡住陈旧锁的删除接管）。
//
// 锁文件已存在时按平台分派：
//   - POSIX：kill(pid, 0) 探活。进程存活（含 EPERM——进程存在但属其他用户）→
//     报"已有 AgentGo 实例在运行 (pid=N)"；ESRCH → 陈旧锁，删除后接管；其他
//     错误 → 无法判定，中止并提示用户。内容缺失/无法解析视为上次崩溃遗留
//     （创建与写内容之间存在极小窗口），按陈旧锁删除接管。
//   - Windows：os.FindProcess 不校验进程真实性（恒成功），探活无意义，一律
//     跳过——只要锁存在（无论内容是否可解析）就中止，错误给出锁路径（与可
//     解析时的 pid），提示用户确认无实例运行后手动删除。
//
// 返回的 release 幂等：仅当锁文件记录的仍是自己的 pid 时才删除（防止误删
// 接管者的新锁）。进程崩溃未 release 时由下一次启动的陈旧锁路径兜底。
func acquireInstanceLock(agentgoDir string) (release func(), err error) {
	if err := os.MkdirAll(agentgoDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 .agentgo 目录失败 (%s): %w", agentgoDir, err)
	}
	lockPath := filepath.Join(agentgoDir, instanceLockFileName)
	ownPID := os.Getpid()

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			if _, werr := f.WriteString(lockFileContent(ownPID)); werr != nil {
				_ = f.Close()
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("写入实例锁文件失败 (%s): %w", lockPath, werr)
			}
			if cerr := f.Close(); cerr != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("关闭实例锁文件失败 (%s): %w", lockPath, cerr)
			}
			var once sync.Once
			return func() {
				once.Do(func() { releaseInstanceLock(lockPath, ownPID) })
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("创建实例锁文件失败 (%s): %w", lockPath, err)
		}

		// 锁已存在：读出记录的持有者 pid，按平台分派。
		holderPID, readErr := readLockHolderPID(lockPath)
		if runtime.GOOS == "windows" {
			// Windows 的 os.FindProcess 不校验进程真实性（恒成功），探活无意义
			// ——任何现存锁一律按"无法确认"中止，错误指明锁路径并提示删除。
			if readErr == nil {
				return nil, fmt.Errorf("检测到实例锁文件 %s（记录 pid=%d）——Windows 上无法可靠判定该进程是否存活，已中止启动；如确认无 AgentGo 实例在运行（陈旧锁），请删除该文件后重试", lockPath, holderPID)
			}
			return nil, fmt.Errorf("检测到实例锁文件 %s（内容无法解析: %v）——无法确认持有者是否存活，已中止启动；如确认无 AgentGo 实例在运行（陈旧锁），请删除该文件后重试", lockPath, readErr)
		}
		if readErr == nil {
			alive, aliveErr := pidAlive(holderPID)
			switch {
			case alive:
				return nil, fmt.Errorf("已有 AgentGo 实例在运行 (pid=%d)，同一项目目录只允许一个实例；如确认该进程已退出，请删除锁文件 %s 后重试", holderPID, lockPath)
			case aliveErr != nil:
				return nil, fmt.Errorf("无法确认实例锁持有者 (pid=%d) 的存活状态: %v；如确认无 AgentGo 实例在运行，请删除锁文件 %s 后重试", holderPID, aliveErr, lockPath)
			}
			// ESRCH：陈旧锁，落入下方删除后重试接管。
		}
		// POSIX：陈旧锁（ESRCH）或内容无法解析（崩溃在创建与写入之间的遗留）：
		// 删除后下一轮重试创建。
		if rmErr := os.Remove(lockPath); rmErr != nil {
			return nil, fmt.Errorf("移除陈旧实例锁失败 (%s): %w", lockPath, rmErr)
		}
	}
	return nil, fmt.Errorf("实例锁竞争未解决 (%s)：另一实例可能正在启动，请稍后重试", lockPath)
}

// lockFileContent 生成锁文件内容：pid + 主机名 + 启动时间（供人工排查识别）。
func lockFileContent(pid int) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("pid=%d\nhost=%s\nstarted=%s\n", pid, host, time.Now().Format(time.RFC3339))
}

// readLockHolderPID 读取锁文件中记录的持有者 pid；文件不存在、不可读或缺少
// 合法 pid 字段时返回错误（调用方按"无法确认持有者"处理）。
func readLockHolderPID(lockPath string) (int, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "pid=")
		if !ok {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(rest))
		if perr != nil || pid <= 0 {
			return 0, fmt.Errorf("锁文件 pid 字段无法解析: %q", rest)
		}
		return pid, nil
	}
	return 0, fmt.Errorf("锁文件缺少 pid 字段")
}

// pidAlive 用 signal-0 探测进程是否存在。仅在非 Windows 平台调用（Windows 的
// os.FindProcess 恒成功，探活无意义，调用方已按平台分流）。
// 返回 (true, nil)=存活；(false, nil)=不存在（ESRCH，陈旧锁）；
// (false, err)=无法判定。EPERM 表示进程存在但属其他用户，按存活处理。
func pidAlive(pid int) (bool, error) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	// Go 的 Unix os.Process 在已知进程不存在时可能先把 ESRCH 归一化为
	// os.ErrProcessDone（WSL/Linux 上的错误文本为 "os: process already
	// finished"）。它与原始 ESRCH 语义相同：锁持有者已经退出。
	if errors.Is(err, os.ErrProcessDone) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, err
}

// releaseInstanceLock 删除锁文件，但仅当文件中记录的仍是自己的 pid——防止
// "本实例锁被判陈旧、已被新实例接管"的极端场景下误删新实例的锁。
func releaseInstanceLock(lockPath string, ownPID int) {
	holderPID, err := readLockHolderPID(lockPath)
	if err != nil || holderPID != ownPID {
		return // 文件已不存在/不可读，或已被接管——都不该动
	}
	_ = os.Remove(lockPath)
}
