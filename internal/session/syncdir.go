package session

import (
	"log"
	"os"
	"runtime"
)

// syncDir 对目录做 fsync：tmp+rename 原子写只保证文件内容落盘，rename 本身
// 修改的是【目录项】——不对目录 fsync，掉电可能丢掉整个 rename（F11）。
//
// best-effort 语义：任何失败只打 WARN，绝不向上返回致命错误——rename 已经
// 完成，这里丢的只是一层额外的耐久性保证。
// Windows 上目录句柄的 Sync 恒失败（FlushFileBuffers 不接受目录），该平台
// 直接整体跳过（避免每次保存都刷一条预期内 WARN）。
// 句柄在函数返回前即关闭（Windows 文件锁规则：不留长驻句柄阻塞 TempDir 清理）。
func syncDir(dirPath string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dirPath)
	if err != nil {
		log.Printf("[WARNING] syncDir 打开目录失败 %s: %v", dirPath, err)
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		log.Printf("[WARNING] syncDir 同步目录失败 %s: %v", dirPath, err)
		return err
	}
	return nil
}
