package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 本文件实现启动恢复（V6 §6-13）：先读最近一份完整 snapshot，再按 seq
// 严格递增重放其后的 journal 条目，重建内存 GraphDocument，校验 schema、
// revision/state_version 单调与 digest 一致后才入索引恢复读写。
//
// 损坏处理：
//   - journal 坏行即停：该行及其后物理截断丢弃，以最后一个完整一致状态
//     为准，并在 Recover 返回的错误/告警中说明；
//   - snapshot 损坏/缺失：尝试纯 journal 重放（首条必须是 seq=1 的
//     submit；压缩截断过的 journal 不满足此条件，此时该图不可恢复）。

// Recover 扫描持久化根目录逐图恢复。单图失败不影响其它图；所有告警与
// 失败经 errors.Join 汇总返回（图仍可能已成功恢复，调用方按图查询）。
// 内存索引中已存在的 graph_id 跳过（Recover 设计为启动期调用，内存优先）。
//
// 图目录经递归走查发现：含 journal.jsonl 或 snapshot.json 的目录即图目录，
// graph_id = 相对根目录的 "/" 分段路径——subgraph 物化子图的目录嵌套在
// 父图目录下（<父图ID>/<activationID>），必须与顶层图一并恢复。
func (s *Store) Recover() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	graphDirs, err := discoverGraphDirs(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("graph: 扫描持久化根目录: %w", err)
	}
	var errs []error
	for _, ref := range graphDirs {
		if _, ok := s.entries[ref.GraphID]; ok {
			continue
		}
		rec, err := s.recoverGraph(ref.GraphID, ref.Dir)
		if rec != nil {
			s.entries[ref.GraphID] = rec
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// graphDirRef 同时保存解码后的逻辑身份与实际物理目录；含 ':'、Windows
// 设备名等 graph_id 分段会经过 path.go 的可逆编码，不能再用逻辑 ID 直接
// filepath.Join 回磁盘。
type graphDirRef struct {
	GraphID string
	Dir     string
}

// discoverGraphDirs 递归发现根目录下的全部图目录（含 journal.jsonl 或
// snapshot.json 的目录），解码各物理分段并按逻辑 graph_id 排序（保证父图
// 先于嵌套子图）。非图目录不恢复但仍下钻。
func discoverGraphDirs(root string) ([]graphDirRef, error) {
	var refs []graphDirRef
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == root {
			return nil
		}
		if hasGraphFiles(path) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			physicalSegments := strings.Split(filepath.ToSlash(rel), "/")
			logicalSegments := make([]string, len(physicalSegments))
			for i, seg := range physicalSegments {
				decoded, err := decodeGraphStorageSegment(seg)
				if err != nil {
					return err
				}
				logicalSegments[i] = decoded
			}
			graphID := strings.Join(logicalSegments, "/")
			if err := validateGraphID(graphID); err != nil {
				return fmt.Errorf("graph: 持久化目录 %q 对应非法 graph_id: %w", rel, err)
			}
			refs = append(refs, graphDirRef{GraphID: graphID, Dir: path})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].GraphID < refs[j].GraphID })
	return refs, nil
}

// hasGraphFiles 报告目录是否含图持久化文件（journal 或 snapshot）。
func hasGraphFiles(dir string) bool {
	for _, name := range []string{journalFileName, snapshotFileName} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// recoverGraph 恢复单张图。返回 nil entry 表示该目录无可恢复内容；
// 返回的 error 含恢复过程中的告警（图本身可能已正常恢复）。
func (s *Store) recoverGraph(graphID, dir string) (*entry, error) {
	var (
		doc        *GraphDocument
		seq        int64
		snapDigest string
		warns      []string
	)
	// Graph Runtime 的 entry 级簿记：snapshot 存全量作底，journal 重放叠加。
	transitions := make(map[transitionKey]TransitionRecord)
	activationSeq := make(map[string]int)

	// 1) snapshot：损坏/缺失则从空开始，尝试纯 journal 重放。
	snap, snapErr := readSnapshot(filepath.Join(dir, snapshotFileName))
	switch {
	case snapErr == nil:
		if err := checkSnapshot(snap, graphID); err != nil {
			warns = append(warns, fmt.Sprintf("snapshot 校验失败（%v），尝试纯 journal 重放", err))
		} else {
			doc, seq, snapDigest = snap.Doc, snap.Seq, snap.Digest
			maps.Copy(activationSeq, snap.ActivationSeq)
			for _, rec := range snap.Transitions {
				transitions[transitionKey{rec.SourceActivationID, rec.TransitionID}] = rec
				noteActivation(activationSeq, rec.TargetNodeID, rec.TargetActivationID)
			}
		}
	case errors.Is(snapErr, os.ErrNotExist):
		// 无 snapshot：纯 journal 重放（未触发过压缩的常态）。
	default:
		warns = append(warns, fmt.Sprintf("snapshot 读取失败（%v），尝试纯 journal 重放", snapErr))
	}

	// 2) journal 重放：只放 seq > snapshot.seq 的条目；坏行即停并截断。
	res, err := replayJournal(filepath.Join(dir, journalFileName), doc, seq, transitions, activationSeq)
	if err != nil {
		return nil, fmt.Errorf("graph: 恢复图 %s: %w", graphID, err)
	}
	if res.corruptErr != nil {
		warns = append(warns, fmt.Sprintf("journal 坏行即停: %v（该行及其后已截断丢弃）", res.corruptErr))
	}
	doc = res.doc

	if doc == nil {
		if len(warns) == 0 {
			return nil, nil // 空目录（如 SubmitGraph 落盘失败的残留），静默跳过
		}
		return nil, fmt.Errorf("graph: 图 %s 无可恢复内容: %s", graphID, strings.Join(warns, "；"))
	}

	// 3) 终态校验：schema/语义/状态枚举 + digest 与日志尾（或 snapshot）对照。
	if doc.GraphID != graphID {
		return nil, fmt.Errorf("graph: 图 %s 恢复内容的 graph_id=%q 与目录名不符", graphID, doc.GraphID)
	}
	if err := validateSemantics(doc); err != nil {
		return nil, fmt.Errorf("graph: 图 %s 恢复后语义校验失败: %w", graphID, err)
	}
	if err := validateRuntimeState(doc); err != nil {
		return nil, fmt.Errorf("graph: 图 %s 恢复后状态校验失败: %w", graphID, err)
	}
	wantDigest := res.lastDigest
	if wantDigest == "" {
		wantDigest = snapDigest // 无 journal 条目时与 snapshot 对照
	}
	if got := ComputeDigest(doc); wantDigest != "" && got != wantDigest {
		return nil, fmt.Errorf("graph: 图 %s 恢复后 digest 不一致：重算 %s，日志尾/快照 %s",
			graphID, got, wantDigest)
	}

	// 4) 打开 journal 追加写，入索引；恢复出的图可立即正常读写。
	jw, err := openJournal(filepath.Join(dir, journalFileName), false)
	if err != nil {
		return nil, fmt.Errorf("graph: 图 %s 打开 journal: %w", graphID, err)
	}
	e := &entry{
		doc:            doc,
		journal:        jw,
		seq:            res.seq,
		digest:         ComputeDigest(doc),
		journalEntries: res.applied,
		journalBytes:   res.fileBytes,
		dir:            dir,
		transitions:    transitions,
		activationSeq:  activationSeq,
	}
	if len(warns) > 0 {
		return e, fmt.Errorf("graph: 图 %s 恢复有告警: %s", graphID, strings.Join(warns, "；"))
	}
	return e, nil
}

// readSnapshot 读取并解析 snapshot.json。
func readSnapshot(path string) (*snapshotFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("解析 snapshot: %w", err)
	}
	return &snap, nil
}

// checkSnapshot 校验 snapshot 内容自洽且属于 graphID。
func checkSnapshot(snap *snapshotFile, graphID string) error {
	if snap.Version != snapshotVersion {
		return fmt.Errorf("snapshot 版本 %d 未知", snap.Version)
	}
	if snap.GraphID != graphID {
		return fmt.Errorf("snapshot graph_id=%q 与目录名 %q 不符", snap.GraphID, graphID)
	}
	if snap.Doc == nil {
		return fmt.Errorf("snapshot 缺少 doc")
	}
	if snap.Doc.GraphID != graphID {
		return fmt.Errorf("snapshot doc.graph_id=%q 与目录名 %q 不符", snap.Doc.GraphID, graphID)
	}
	if snap.Revision != snap.Doc.Revision || snap.StateVersion != snap.Doc.StateVersion {
		return fmt.Errorf("snapshot 游标（revision=%d state_version=%d）与 doc（revision=%d state_version=%d）不符",
			snap.Revision, snap.StateVersion, snap.Doc.Revision, snap.Doc.StateVersion)
	}
	if got := ComputeDigest(snap.Doc); got != snap.Digest {
		return fmt.Errorf("snapshot digest 不一致：重算 %s，记录 %s", got, snap.Digest)
	}
	return nil
}

// replayResult 是 journal 重放的结果。
type replayResult struct {
	doc        *GraphDocument // 重放后的文档（无有效条目时为 base，可能 nil）
	seq        int64          // 最后完整条目 seq（无条目则 = base seq）
	lastDigest string         // 最后完整条目的 digest（无条目为空）
	applied    int64          // 实际重放的条目数（不含 snapshot 已含的跳过项）
	fileBytes  int64          // 有效前缀字节数（坏行截断后）
	corruptErr error          // 坏行原因（未发生为 nil）
}

// replayJournal 按 seq 顺序重放 journal 中 seq > baseSeq 的条目。
// 每条条目三重对照：seq 严格连续、revision/state_version 与重算一致、
// digest 与重算一致（含日志尾）；任一不符即按坏行处理：停止重放并把
// 该行及其后物理截断丢弃，以最后一个完整一致状态为准。
// transitions/activationSeq 是 Graph Runtime 的 entry 级簿记底稿（来自
// snapshot），重放中就地叠加：transition 记录进边选择集合，execution 系
// 记录推进 activation 序号。
func replayJournal(path string, base *GraphDocument, baseSeq int64, transitions map[transitionKey]TransitionRecord, activationSeq map[string]int) (*replayResult, error) {
	res := &replayResult{doc: base, seq: baseSeq}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读 journal: %w", err)
	}
	res.fileBytes = int64(len(data))
	var (
		offset   int64
		goodLen  int64
		expected = baseSeq + 1
	)
	for offset < int64(len(data)) {
		var line []byte
		if nl := bytes.IndexByte(data[offset:], '\n'); nl < 0 {
			line = data[offset:]
			offset = int64(len(data))
		} else {
			line = data[offset : offset+int64(nl)]
			offset += int64(nl) + 1
		}
		line = bytes.TrimSuffix(line, []byte{'\r'}) // 边界处归一可能的 CRLF
		if len(bytes.TrimSpace(line)) == 0 {
			goodLen = offset // 空行容忍（如文件末尾）
			continue
		}
		var je journalEntry
		if err := json.Unmarshal(line, &je); err != nil {
			res.corruptErr = fmt.Errorf("偏移 %d 处坏行（JSON 解析失败: %v）", goodLen, err)
			break
		}
		if je.Seq <= baseSeq {
			goodLen = offset // snapshot 已含的条目，跳过
			continue
		}
		if je.Seq != expected {
			res.corruptErr = fmt.Errorf("seq 不连续：期望 %d，实际 %d", expected, je.Seq)
			break
		}
		next, err := applyJournalRecord(res.doc, &je)
		if err != nil {
			res.corruptErr = fmt.Errorf("seq %d 应用失败: %v", je.Seq, err)
			break
		}
		noteJournalBookkeeping(next, &je, transitions, activationSeq)
		if je.Digest != "" {
			if got := ComputeDigest(next); got != je.Digest {
				res.corruptErr = fmt.Errorf("seq %d digest 不一致：记录 %s，重算 %s", je.Seq, je.Digest, got)
				break
			}
		}
		res.doc = next
		res.seq = je.Seq
		res.lastDigest = je.Digest
		res.applied++
		expected = je.Seq + 1
		goodLen = offset
	}
	if res.corruptErr != nil {
		// 坏行即停：该行及其后物理截断丢弃（此时 journal 尚未打开追加写）。
		if err := os.Truncate(path, goodLen); err != nil {
			return res, fmt.Errorf("截断坏行失败: %w（原错误: %v）", err, res.corruptErr)
		}
		res.fileBytes = goodLen
	}
	return res, nil
}

// applyJournalRecord 把一条 journal 记录应用到 doc（doc 为恢复私有，原地
// 修改）。恢复信任写入时的状态机校验结果，不重复迁移校验；一致性由
// revision/state_version 重算比对与 digest 对照保证。
func applyJournalRecord(doc *GraphDocument, je *journalEntry) (*GraphDocument, error) {
	switch je.Kind {
	case journalKindSubmit:
		if doc != nil {
			return nil, fmt.Errorf("重复 submit 记录")
		}
		var p submitPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 submit payload: %w", err)
		}
		if p.Doc == nil {
			return nil, fmt.Errorf("submit payload 缺少 doc")
		}
		doc = p.Doc
	case journalKindPatch:
		if doc == nil {
			return nil, fmt.Errorf("patch 记录前缺少 submit")
		}
		var p patchPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 patch payload: %w", err)
		}
		if err := applyPatch(doc, &p.Patch); err != nil {
			return nil, err
		}
		doc.Revision++
		doc.StateVersion++
	case journalKindNodeStatus:
		var p nodeStatusPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 node_status payload: %w", err)
		}
		n, err := journalNode(doc, p.NodeID, je.Kind)
		if err != nil {
			return nil, err
		}
		n.Status = p.To
		doc.Nodes[p.NodeID] = n
		doc.StateVersion++
	case journalKindExecutor:
		var p executorPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 executor payload: %w", err)
		}
		n, err := journalNode(doc, p.NodeID, je.Kind)
		if err != nil {
			return nil, err
		}
		ex := p.Executor
		n.Executor = &ex
		doc.Nodes[p.NodeID] = n
		doc.StateVersion++
	case journalKindExecution:
		var p executionPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 execution payload: %w", err)
		}
		n, err := journalNode(doc, p.NodeID, je.Kind)
		if err != nil {
			return nil, err
		}
		ex := p.Execution
		n.Execution = &ex
		doc.Nodes[p.NodeID] = n
		doc.StateVersion++
	case journalKindGraphStatus:
		if doc == nil {
			return nil, fmt.Errorf("graph_status 记录前缺少 submit")
		}
		var p graphStatusPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 graph_status payload: %w", err)
		}
		doc.Status = p.To
		doc.StateVersion++
	case journalKindExecutionStatus:
		var p executionStatusPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 execution_status payload: %w", err)
		}
		n, err := journalNode(doc, p.NodeID, je.Kind)
		if err != nil {
			return nil, err
		}
		ex := p.Execution
		n.Execution = &ex
		n.Status = p.To
		doc.Nodes[p.NodeID] = n
		doc.StateVersion++
	case journalKindTransition:
		if doc == nil {
			return nil, fmt.Errorf("transition 记录前缺少 submit")
		}
		var p transitionPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 transition payload: %w", err)
		}
		// 边选择集合本身由 noteJournalBookkeeping 在重放侧重建；
		// 这里只推进 state_version 以保持版本对照一致。
		doc.StateVersion++
	default:
		return nil, fmt.Errorf("未知 journal 类型 %q", je.Kind)
	}
	// revision/state_version 单调一致性：重算值必须与记录一致。
	if doc.Revision != je.Revision || doc.StateVersion != je.StateVersion {
		return nil, fmt.Errorf("版本不一致：记录 revision=%d state_version=%d，重放得到 revision=%d state_version=%d",
			je.Revision, je.StateVersion, doc.Revision, doc.StateVersion)
	}
	return doc, nil
}

// journalNode 取重放目标节点；doc 为 nil 或节点不存在即日志损坏。
func journalNode(doc *GraphDocument, nodeID, kind string) (Node, error) {
	if doc == nil {
		return Node{}, fmt.Errorf("%s 记录前缺少 submit", kind)
	}
	n, ok := doc.Nodes[nodeID]
	if !ok {
		return Node{}, fmt.Errorf("%s 记录指向不存在的节点 %q", kind, nodeID)
	}
	return n, nil
}

// noteJournalBookkeeping 把一条已成功重放的 journal 记录中的 entry 级簿记
// （边选择记录 / activation 序号）叠加到底稿上。payload 解析失败不影响
// 主重放（applyJournalRecord 已校验过同一份 payload 的结构）。
func noteJournalBookkeeping(doc *GraphDocument, je *journalEntry, transitions map[transitionKey]TransitionRecord, activationSeq map[string]int) {
	switch je.Kind {
	case journalKindSubmit:
		for id, n := range doc.Nodes {
			if n.Execution != nil {
				noteActivation(activationSeq, id, n.Execution.ActivationID)
			}
		}
	case journalKindExecution:
		var p executionPayload
		if err := json.Unmarshal(je.Payload, &p); err == nil {
			noteActivation(activationSeq, p.NodeID, p.Execution.ActivationID)
		}
	case journalKindExecutionStatus:
		var p executionStatusPayload
		if err := json.Unmarshal(je.Payload, &p); err == nil {
			noteActivation(activationSeq, p.NodeID, p.Execution.ActivationID)
		}
	case journalKindTransition:
		var p transitionPayload
		if err := json.Unmarshal(je.Payload, &p); err == nil {
			rec := TransitionRecord(p)
			transitions[transitionKey{p.SourceActivationID, p.TransitionID}] = rec
			noteActivation(activationSeq, rec.TargetNodeID, rec.TargetActivationID)
		}
	}
}
