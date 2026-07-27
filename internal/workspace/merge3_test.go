package workspace

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestMerge3NoChange(t *testing.T) {
	base := []byte("a\nb\nc\n")
	merged, conflicts, ok := Merge3(base, base, base)
	if !ok {
		t.Fatalf("无变更应合并成功")
	}
	if len(conflicts) != 0 {
		t.Fatalf("无变更不应有冲突，得到 %d 个", len(conflicts))
	}
	if string(merged) != string(base) {
		t.Fatalf("无变更合并结果应等于基线，得到 %q", merged)
	}
}

func TestMerge3OneSideChange(t *testing.T) {
	base := []byte("a\nb\nc\n")
	changed := []byte("a\nB\nc\n")

	merged, conflicts, ok := Merge3(base, changed, base)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("main 单边变更应自动合并，conflicts=%v", conflicts)
	}
	if string(merged) != "a\nB\nc\n" {
		t.Fatalf("main 单边变更未正确应用：%q", merged)
	}

	merged, conflicts, ok = Merge3(base, base, changed)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("ours 单边变更应自动合并，conflicts=%v", conflicts)
	}
	if string(merged) != "a\nB\nc\n" {
		t.Fatalf("ours 单边变更未正确应用：%q", merged)
	}
}

func TestMerge3DisjointAutoMerge(t *testing.T) {
	base := []byte("l1\nl2\nl3\nl4\nl5\n")
	main := []byte("L1\nl2\nl3\nl4\nl5\n") // 改第 1 行
	ours := []byte("l1\nl2\nl3\nl4\nL5\n") // 改第 5 行
	merged, conflicts, ok := Merge3(base, main, ours)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("不相交变更应自动合并，conflicts=%v", conflicts)
	}
	want := "L1\nl2\nl3\nl4\nL5\n"
	if string(merged) != want {
		t.Fatalf("自动合并结果错误：得到 %q 期望 %q", merged, want)
	}
}

func TestMerge3OverlappingConflict(t *testing.T) {
	base := []byte("a\nb\nc\n")
	main := []byte("a\nMAIN\nc\n")
	ours := []byte("a\nOURS\nc\n")
	merged, conflicts, ok := Merge3(base, main, ours)
	if ok {
		t.Fatalf("相交变更应报冲突")
	}
	if len(conflicts) != 1 {
		t.Fatalf("应有 1 个冲突区域，得到 %d", len(conflicts))
	}
	c := conflicts[0]
	if c.BaseStart != 2 || c.BaseEnd != 2 {
		t.Fatalf("冲突区域行号错误：[%d,%d]，期望 [2,2]", c.BaseStart, c.BaseEnd)
	}
	if c.Main != "MAIN" || c.Workspace != "OURS" {
		t.Fatalf("冲突文本错误：main=%q workspace=%q", c.Main, c.Workspace)
	}
	if !strings.Contains(string(merged), "<<<<<<< main") ||
		!strings.Contains(string(merged), ">>>>>>> workspace") {
		t.Fatalf("合并结果应包含冲突标记：%q", merged)
	}
}

func TestMerge3IdenticalChangeAdopted(t *testing.T) {
	base := []byte("a\nb\nc\n")
	main := []byte("a\nB\nc\n")
	ours := []byte("a\nB\nc\n")
	merged, conflicts, ok := Merge3(base, main, ours)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("双侧相同变更应自动采用，conflicts=%v", conflicts)
	}
	if string(merged) != "a\nB\nc\n" {
		t.Fatalf("相同变更应采用一次：%q", merged)
	}
}

func TestMerge3DoubleInsertConflict(t *testing.T) {
	base := []byte("a\nb\n")
	main := []byte("a\nX\nb\n")
	ours := []byte("a\nY\nb\n")
	_, conflicts, ok := Merge3(base, main, ours)
	if ok {
		t.Fatalf("同位置双插入不同内容应报冲突")
	}
	if len(conflicts) != 1 {
		t.Fatalf("应有 1 个冲突区域，得到 %d", len(conflicts))
	}
	if conflicts[0].Main != "X" || conflicts[0].Workspace != "Y" {
		t.Fatalf("冲突文本错误：main=%q workspace=%q",
			conflicts[0].Main, conflicts[0].Workspace)
	}

	// 同位置同内容插入 → 自动采用。
	merged, conflicts, ok := Merge3(base, main, main)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("同位置同内容插入应自动采用，conflicts=%v", conflicts)
	}
	if string(merged) != "a\nX\nb\n" {
		t.Fatalf("相同插入应采用一次：%q", merged)
	}
}

func TestMerge3AppendAtEOF(t *testing.T) {
	base := []byte("a\nb\n")
	main := []byte("A\nb\n")    // 改首行
	ours := []byte("a\nb\nc\n") // 文件尾新增
	merged, conflicts, ok := Merge3(base, main, ours)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("文件尾新增与首行修改应自动合并，conflicts=%v", conflicts)
	}
	if string(merged) != "A\nb\nc\n" {
		t.Fatalf("合并结果错误：%q", merged)
	}
}

func TestMerge3LargeFile(t *testing.T) {
	// 约 1MB：3 万行 × 36 字节，双侧各一处不相交变更。
	var b strings.Builder
	for i := 0; i < 30000; i++ {
		fmt.Fprintf(&b, "line %05d padding padding pad\n", i)
	}
	base := []byte(b.String())
	main := bytes.Replace(base, []byte("line 00000"), []byte("MAIN FIRST"), 1)
	ours := append([]byte(nil), base...)
	ours = append(ours, []byte("tail added by workspace\n")...)

	merged, conflicts, ok := Merge3(base, main, ours)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("大文件不相交变更应自动合并，conflicts=%d", len(conflicts))
	}
	if !bytes.HasPrefix(merged, []byte("MAIN FIRST")) {
		t.Fatalf("大文件合并结果缺少 main 侧变更")
	}
	if !bytes.HasSuffix(merged, []byte("tail added by workspace\n")) {
		t.Fatalf("大文件合并结果缺少 workspace 侧变更")
	}
}

func TestMerge3CRLineEndingsPreserved(t *testing.T) {
	// 行内容保留 \r，不做特殊 CRLF 处理。
	base := []byte("a\r\nb\r\n")
	ours := []byte("a\r\nB\r\n")
	merged, conflicts, ok := Merge3(base, base, ours)
	if !ok || len(conflicts) != 0 {
		t.Fatalf("单边 CRLF 变更应自动合并，conflicts=%v", conflicts)
	}
	if string(merged) != "a\r\nB\r\n" {
		t.Fatalf("CRLF 内容未原样保留：%q", merged)
	}
}
