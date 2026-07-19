package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestAcquireInstanceLock_DoubleAcquireFails：同一 .agentgo 目录第二次抢锁必须
// 失败，且错误中携带持有者 pid 与锁文件路径（POSIX 走探活判活路径，Windows 走
// "无法探活即中止"路径——两条路径的错误都含这两者）。
func TestAcquireInstanceLock_DoubleAcquireFails(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("首次获取锁失败: %v", err)
	}
	t.Cleanup(release)

	if _, err := acquireInstanceLock(dir); err == nil {
		t.Fatal("第二次获取锁应失败")
	} else {
		if pid := strconv.Itoa(os.Getpid()); !strings.Contains(err.Error(), pid) {
			t.Errorf("错误应携带持有者 pid=%s: %v", pid, err)
		}
		if lockPath := filepath.Join(dir, instanceLockFileName); !strings.Contains(err.Error(), lockPath) {
			t.Errorf("错误应指出锁文件路径 %s: %v", lockPath, err)
		}
	}
}

// TestAcquireInstanceLock_ReleaseThenReacquire：release 删除锁文件后可重新获取；
// release 幂等（重复调用不 panic、不报错）。
func TestAcquireInstanceLock_ReleaseThenReacquire(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("首次获取锁失败: %v", err)
	}
	release()
	release() // 幂等：重复 release 安全

	if _, err := os.Stat(filepath.Join(dir, instanceLockFileName)); !os.IsNotExist(err) {
		t.Fatalf("release 后锁文件应被删除: stat err=%v", err)
	}

	release2, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("release 后重新获取锁失败: %v", err)
	}
	t.Cleanup(release2)
}

// TestAcquireInstanceLock_StaleLock：陈旧锁（写入一个必然不存在的 pid）按平台
// 分派——POSIX 探活判死（ESRCH）后接管并换成自己的 pid；Windows 无法探活，
// 中止并在错误中指明锁路径、提示用户删除。
func TestAcquireInstanceLock_StaleLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, instanceLockFileName)
	// 99999999 超过常见 pid_max（Linux 默认 4194304），signal-0 必返回 ESRCH。
	if err := os.WriteFile(lockPath, []byte("pid=99999999\nhost=stale-host\nstarted=2020-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireInstanceLock(dir)
	if runtime.GOOS == "windows" {
		if err == nil {
			release()
			t.Fatal("Windows 上存在锁文件时应中止启动")
		}
		if !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "删除") {
			t.Errorf("Windows 中止错误应指明锁路径并提示删除: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(lockPath) }) // 失败路径不清理锁，测试自己兜底
		return
	}

	// POSIX：陈旧锁被接管——获取成功，锁文件内容换成自己的 pid。
	if err != nil {
		t.Fatalf("陈旧锁应被接管: %v", err)
	}
	t.Cleanup(release)
	holder, herr := readLockHolderPID(lockPath)
	if herr != nil || holder != os.Getpid() {
		t.Errorf("接管后锁持有者应是自己 pid=%d: holder=%d err=%v", os.Getpid(), holder, herr)
	}
}

func TestPIDAlive_NonexistentProcessIsNotAnUnknownError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 pidAlive 探活")
	}
	alive, err := pidAlive(99999999)
	if err != nil || alive {
		t.Fatalf("pidAlive(nonexistent) = (%v, %v), want (false, nil)", alive, err)
	}
}

// TestAcquireInstanceLock_CorruptLock：内容无法解析的锁文件（崩溃在创建与写入
// 之间的遗留）。POSIX 按陈旧锁删除接管；Windows 按"无法确认"中止并提示删除。
func TestAcquireInstanceLock_CorruptLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, instanceLockFileName)
	if err := os.WriteFile(lockPath, []byte("garbage-without-pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireInstanceLock(dir)
	if runtime.GOOS == "windows" {
		if err == nil {
			release()
			t.Fatal("Windows 上内容无法解析的锁应中止启动")
		}
		if !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "删除") {
			t.Errorf("Windows 中止错误应指明锁路径并提示删除: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(lockPath) })
		return
	}

	if err != nil {
		t.Fatalf("POSIX 上无法解析的锁应视为崩溃遗留被接管: %v", err)
	}
	t.Cleanup(release)
	holder, herr := readLockHolderPID(lockPath)
	if herr != nil || holder != os.Getpid() {
		t.Errorf("接管后锁持有者应是自己 pid=%d: holder=%d err=%v", os.Getpid(), holder, herr)
	}
}
