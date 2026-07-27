package workspace

import (
	"strings"
)

// maxEditDistance 是单侧 diff 的编辑距离上限（Myers O(ND) 回溯需要
// O(D) 行 V 数组快照，内存与 D² 成正比）。超过上限说明该侧近乎整体重写，
// 退化为「整文件单个变更」——对合并而言与真实 hunk 划分等价（必然与对侧
// 任何变更相交），但内存保持有界。1MB 级文件的常规编辑（D 个位数到
// 数百）远低于上限，性能毫秒级。
const maxEditDistance = 2048

// Merge3 行级三路合并：base 为基线，main 为主根当前内容，ours 为
// workspace 内容。纯函数，便于单测。
//
// 算法：分别用 Myers O(ND) diff 求 base→main 与 base→ours 的行级变更
// 区间；区间不相交的双侧变更都应用；相交且两侧变更文本完全一致的视为
// 同一变更自动采用；其余相交情形记为冲突。ok=false 时 merged 仍返回，
// 但内含「<<<<<<< main / ======= / >>>>>>> workspace」冲突标记块，
// 仅供诊断展示，调用方不得落盘。
//
// 行按 "\n" 切分，行内容保留 \r（不做特殊 CRLF 处理）。
func Merge3(base, main, ours []byte) (merged []byte, conflicts []ConflictRegion, ok bool) {
	baseL := splitLines(base)
	mainL := splitLines(main)
	oursL := splitLines(ours)

	mc := diffLines(baseL, mainL)
	oc := diffLines(baseL, oursL)

	var out []string
	i := 0 // base 行游标（下一未处理行）
	mi, oi := 0, 0
	for {
		var mNext, oNext *lineChange
		if mi < len(mc) {
			mNext = &mc[mi]
		}
		if oi < len(oc) {
			oNext = &oc[oi]
		}
		if mNext == nil && oNext == nil {
			out = append(out, baseL[i:]...)
			break
		}
		if mNext != nil && oNext != nil && changesOverlap(*mNext, *oNext) {
			// 收集完整冲突簇：相交变更可能连锁重叠，一并并入。
			lo := min(mNext.lo, oNext.lo)
			hi := max(mNext.hi, oNext.hi)
			mStart, oStart := mi, oi
			mi++
			oi++
			for {
				grown := false
				for mi < len(mc) && changesOverlap(mc[mi], lineChange{lo: lo, hi: hi}) {
					lo = min(lo, mc[mi].lo)
					hi = max(hi, mc[mi].hi)
					mi++
					grown = true
				}
				for oi < len(oc) && changesOverlap(oc[oi], lineChange{lo: lo, hi: hi}) {
					lo = min(lo, oc[oi].lo)
					hi = max(hi, oc[oi].hi)
					oi++
					grown = true
				}
				if !grown {
					break
				}
			}
			out = append(out, baseL[i:lo]...)
			mainText := applyChanges(baseL[lo:hi], mc[mStart:mi], lo)
			oursText := applyChanges(baseL[lo:hi], oc[oStart:oi], lo)
			if equalLines(mainText, oursText) {
				// 双侧变更完全一致 → 同一变更，自动采用一次。
				out = append(out, mainText...)
			} else {
				conflicts = append(conflicts, ConflictRegion{
					BaseStart: lo + 1, // 1-based；纯插入冲突时 BaseEnd < BaseStart 表示插入点
					BaseEnd:   hi,
					Main:      strings.Join(mainText, "\n"),
					Workspace: strings.Join(oursText, "\n"),
				})
				out = append(out, "<<<<<<< main")
				out = append(out, mainText...)
				out = append(out, "=======")
				out = append(out, oursText...)
				out = append(out, ">>>>>>> workspace")
			}
			i = hi
			continue
		}
		// 单侧变更：位置靠前者先处理；同位置时纯插入优先于修改，保证
		// 「同点插入 + 同点修改」确定性地先插入后修改。
		takeMain := mNext != nil && (oNext == nil ||
			mNext.lo < oNext.lo ||
			(mNext.lo == oNext.lo && (isInsert(*mNext) || !isInsert(*oNext))))
		if takeMain {
			out = appendSingle(out, baseL, &i, *mNext)
			mi++
		} else {
			out = appendSingle(out, baseL, &i, *oNext)
			oi++
		}
	}
	return []byte(strings.Join(out, "\n")), conflicts, len(conflicts) == 0
}

// lineChange 是 base→target 的一处行级变更：base 的 [lo, hi) 行
// （0-based 半开区间）被替换为 lines。lo==hi 表示在 lo 行前纯插入。
type lineChange struct {
	lo, hi int
	lines  []string
}

func isInsert(c lineChange) bool { return c.lo == c.hi }

// changesOverlap 判定分别来自 main / ours 的两处变更是否相交：
// 非空区间按严格相交判定；纯插入（空区间）只在严格落入对方区间内部、
// 或与对方纯插入同点时视为相交。
func changesOverlap(a, b lineChange) bool {
	aIns, bIns := isInsert(a), isInsert(b)
	switch {
	case aIns && bIns:
		return a.lo == b.lo
	case aIns:
		return b.lo < a.lo && a.lo < b.hi
	case bIns:
		return a.lo < b.lo && b.lo < a.hi
	default:
		return max(a.lo, b.lo) < min(a.hi, b.hi)
	}
}

// applyChanges 把一侧落入 base[off:off+len(seg)] 的全部变更应用到 seg
// 上，得到该侧对该区间的完整修改结果。
func applyChanges(seg []string, cs []lineChange, off int) []string {
	var out []string
	cur := 0
	for _, c := range cs {
		s, e := c.lo-off, c.hi-off
		out = append(out, seg[cur:s]...)
		out = append(out, c.lines...)
		cur = e
	}
	return append(out, seg[cur:]...)
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// appendSingle 应用单侧（不相交）变更。防御：变更区间已被前序处理消耗时
// 跳过 base 拷贝与游标前进，仅追加变更行（正常路径不会触发）。
func appendSingle(out []string, base []string, i *int, c lineChange) []string {
	if c.lo > *i {
		out = append(out, base[*i:c.lo]...)
	}
	out = append(out, c.lines...)
	if c.hi > *i {
		*i = c.hi
	}
	return out
}

// splitLines 按 "\n" 切行，行内容保留 \r。Split/Join 往返无损。
func splitLines(b []byte) []string {
	return strings.Split(string(b), "\n")
}

// diffLines 求 base→target 的行级变更序列（按 lo 升序、互不重叠）。
func diffLines(base, target []string) []lineChange {
	// 剥离公共前后缀：加速常见情形，并让 diff 只关注真实变更区。
	s := 0
	for s < len(base) && s < len(target) && base[s] == target[s] {
		s++
	}
	eb, et := len(base), len(target)
	for eb > s && et > s && base[eb-1] == target[et-1] {
		eb--
		et--
	}
	a, b := base[s:eb], target[s:et]
	var core []lineChange
	switch {
	case len(a) == 0 && len(b) == 0:
		return nil
	case len(a) == 0:
		core = []lineChange{{lo: 0, hi: 0, lines: append([]string(nil), b...)}}
	case len(b) == 0:
		core = []lineChange{{lo: 0, hi: len(a)}}
	default:
		core = myersCore(a, b)
	}
	for i := range core {
		core[i].lo += s
		core[i].hi += s
	}
	return core
}

// myersCore 是 Myers O(ND) 差分算法主体：求 a→b 的最短编辑脚本并合并为
// 变更区间。编辑距离超过 maxEditDistance 时退化为整段替换（见常量注释）。
func myersCore(a, b []string) []lineChange {
	n, m := len(a), len(b)
	maxD := n + m
	if maxD > maxEditDistance {
		maxD = maxEditDistance
	}
	offset := maxD
	v := make([]int, 2*maxD+1)
	var trace [][]int
	found := false
	d := 0
	for ; d <= maxD; d++ {
		for k := -d; k <= d; k += 2 {
			ki := k + offset
			var x int
			if k == -d || (k != d && v[ki-1] < v[ki+1]) {
				x = v[ki+1] // 向下：插入
			} else {
				x = v[ki-1] + 1 // 向右：删除
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[ki] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
		row := make([]int, len(v))
		copy(row, v)
		trace = append(trace, row)
		if found {
			break
		}
	}
	if !found {
		// 编辑距离超限：整段替换。
		return []lineChange{{lo: 0, hi: n, lines: append([]string(nil), b...)}}
	}

	// 回溯编辑脚本：del 标记 a 的删除行，ins 记录每个 a 位置前插入的行。
	del := make([]bool, n)
	ins := make([][]string, n+1)
	x, y := n, m
	for dd := d; dd > 0; dd-- {
		prev := trace[dd-1]
		k := x - y
		ki := k + offset
		var prevK int
		if k == -dd || (k != dd && prev[ki-1] < prev[ki+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := prev[prevK+offset]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y-- // snake：相同行
		}
		if x == prevX {
			y--
			ins[x] = append([]string{b[y]}, ins[x]...)
		} else {
			x--
			del[x] = true
		}
	}

	// 把删除标记与插入流合并为变更区间。
	var out []lineChange
	active := false
	var cur lineChange
	flush := func() {
		if active {
			out = append(out, cur)
			active = false
			cur = lineChange{}
		}
	}
	for i := 0; i <= n; i++ {
		hasIns := len(ins[i]) > 0
		hasDel := i < n && del[i]
		if !hasIns && !hasDel {
			flush()
			continue
		}
		if !active {
			active = true
			cur = lineChange{lo: i, hi: i}
		}
		cur.lines = append(cur.lines, ins[i]...)
		if hasDel {
			cur.hi = i + 1
		}
	}
	flush()
	return out
}
