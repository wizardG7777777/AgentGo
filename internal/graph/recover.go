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
	"reflect"
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
		doc              *GraphDocument
		seq              int64
		snapDigest       string
		chainDigest      string
		integrityStarted bool
		warns            []string
	)
	// Graph Runtime 的 entry 级簿记：snapshot 存全量作底，journal 重放叠加。
	transitions := make(map[transitionKey]TransitionRecord)
	activationResults := make(map[string]ActivationResult)
	activationSeq := make(map[string]int)

	// 1) snapshot：损坏/缺失则从空开始，尝试纯 journal 重放。
	snap, snapErr := readSnapshot(filepath.Join(dir, snapshotFileName))
	switch {
	case snapErr == nil:
		if err := checkSnapshot(snap, graphID); err != nil {
			warns = append(warns, fmt.Sprintf("snapshot 校验失败（%v），尝试纯 journal 重放", err))
		} else {
			doc, seq, snapDigest = snap.Doc, snap.Seq, snap.Digest
			snapTransitions, snapResults, snapActivationSeq, _ := snapshotBookkeeping(snap)
			maps.Copy(transitions, snapTransitions)
			maps.Copy(activationResults, snapResults)
			maps.Copy(activationSeq, snapActivationSeq)
			if snap.Version == snapshotVersion {
				chainDigest = snap.ChainDigest
				integrityStarted = true
			} else {
				// v1 没有可信链头；以其完整内容的确定摘要作为 legacy anchor。
				// 后续第一条 v2 journal 会把此前缀封入链中。
				chainDigest = computeSnapshotIntegrityDigest(snap)
			}
		}
	case errors.Is(snapErr, os.ErrNotExist):
		// 无 snapshot：纯 journal 重放（未触发过压缩的常态）。
	default:
		warns = append(warns, fmt.Sprintf("snapshot 读取失败（%v），尝试纯 journal 重放", snapErr))
	}

	// 2) journal 重放：只放 seq > snapshot.seq 的条目；坏行即停并截断。
	res, err := replayJournal(filepath.Join(dir, journalFileName), doc, seq, chainDigest, integrityStarted,
		transitions, activationResults, activationSeq)
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
	if err := validateRecoveredLedger(doc, transitions, activationResults); err != nil {
		return nil, fmt.Errorf("graph: 图 %s 恢复后 ledger 绑定校验失败: %w", graphID, err)
	}

	// 4) 打开 journal 追加写，入索引；恢复出的图可立即正常读写。
	jw, err := openJournal(filepath.Join(dir, journalFileName), false)
	if err != nil {
		return nil, fmt.Errorf("graph: 图 %s 打开 journal: %w", graphID, err)
	}
	e := &entry{
		doc:               doc,
		journal:           jw,
		seq:               res.seq,
		digest:            ComputeDigest(doc),
		chainDigest:       res.chainDigest,
		journalEntries:    res.applied,
		journalBytes:      res.fileBytes,
		dir:               dir,
		transitions:       transitions,
		activationResults: activationResults,
		activationSeq:     activationSeq,
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
	if duplicatePath, key, duplicate := findDuplicateKey(data); duplicate {
		return nil, fmt.Errorf("snapshot 含重复 JSON key %q（path=%s）", key, duplicatePath)
	}
	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("解析 snapshot: %w", err)
	}
	return &snap, nil
}

// checkSnapshot 校验 snapshot 内容自洽且属于 graphID。
func checkSnapshot(snap *snapshotFile, graphID string) error {
	if snap.Version != snapshotVersionLegacy && snap.Version != snapshotVersion {
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
	transitions, results, _, err := snapshotBookkeeping(snap)
	if err != nil {
		return err
	}
	if err := validateRecoveredLedger(snap.Doc, transitions, results); err != nil {
		return fmt.Errorf("snapshot ledger 绑定非法: %w", err)
	}
	if snap.Version == snapshotVersion {
		if strings.TrimSpace(snap.ChainDigest) == "" {
			return fmt.Errorf("snapshot v2 缺少 chain_digest")
		}
		if strings.TrimSpace(snap.IntegrityDigest) == "" {
			return fmt.Errorf("snapshot v2 缺少 integrity_digest")
		}
		if got := computeSnapshotIntegrityDigest(snap); got != snap.IntegrityDigest {
			return fmt.Errorf("snapshot integrity_digest 不一致：重算 %s，记录 %s", got, snap.IntegrityDigest)
		}
	}
	return nil
}

// snapshotBookkeeping 校验并重建 snapshot 的 entry 级账本。相同 identity 的
// 完全相同副本可兼容；同 Ref/key 内容冲突、EvidenceRef 歧义或 owner/bounds
// 非法均拒绝。终态图允许 patch 删除历史节点，因此 snapshot 校验身份归属但
// 不强制旧记录的 node 仍存在于最终 Doc。
func snapshotBookkeeping(snap *snapshotFile) (map[transitionKey]TransitionRecord, map[string]ActivationResult, map[string]int, error) {
	transitions := make(map[transitionKey]TransitionRecord)
	results := make(map[string]ActivationResult)
	activationSeq := maps.Clone(snap.ActivationSeq)
	declaredActivationSeq := maps.Clone(snap.ActivationSeq)
	if activationSeq == nil {
		activationSeq = make(map[string]int)
	}
	for nodeID, n := range activationSeq {
		if strings.TrimSpace(nodeID) == "" || n < 0 {
			return nil, nil, nil, fmt.Errorf("snapshot activation_seq 含非法项 %q=%d", nodeID, n)
		}
	}
	for _, rec := range snap.ActivationResults {
		if err := validateActivationResultRecord(snap.GraphID, nil, rec, false); err != nil {
			return nil, nil, nil, fmt.Errorf("snapshot activation_results 非法: %w", err)
		}
		if err := validateActivationResultAgainstLedger(rec, results); err != nil {
			return nil, nil, nil, fmt.Errorf("snapshot activation_results 冲突: %w", err)
		}
		if previous, exists := results[rec.Ref]; exists && sameActivationResult(previous, rec) {
			continue
		}
		results[rec.Ref] = cloneActivationResult(rec)
		noteActivation(activationSeq, rec.NodeID, rec.ActivationID)
	}
	for _, rec := range snap.Transitions {
		// snapshot v2 可能由“恢复 legacy journal → 后续压缩”产生，记录本身
		// 没有 provenance 位可区分历史形状。随机位翻由 IntegrityDigest 拦截；
		// 新写约束由 live mutate 与 journal v2 的 strict validator 保证。
		if err := validateTransitionRecord(snap.GraphID, nil, rec, results, true); err != nil {
			return nil, nil, nil, fmt.Errorf("snapshot transitions 非法: %w", err)
		}
		key := transitionKey{rec.SourceActivationID, rec.TransitionID}
		if previous, exists := transitions[key]; exists {
			if sameTransitionRecord(previous, rec) {
				continue
			}
			return nil, nil, nil, fmt.Errorf("snapshot transition identity (%s,%d) 内容冲突",
				rec.SourceActivationID, rec.TransitionID)
		}
		transitions[key] = cloneTransitionRecord(rec)
		noteActivation(activationSeq, rec.TargetNodeID, rec.TargetActivationID)
	}
	for id, node := range snap.Doc.Nodes {
		if node.Execution == nil || node.Execution.ActivationID == "" {
			continue
		}
		owner, _, ok := parseActivationID(node.Execution.ActivationID)
		if !ok || owner != id {
			return nil, nil, nil, fmt.Errorf("snapshot 节点 %s 的 execution.activation_id %q owner 不一致", id, node.Execution.ActivationID)
		}
		noteActivation(activationSeq, id, node.Execution.ActivationID)
	}
	if snap.Version == snapshotVersion {
		for nodeID, derived := range activationSeq {
			if declaredActivationSeq[nodeID] < derived {
				return nil, nil, nil, fmt.Errorf("snapshot activation_seq[%q]=%d 小于 ledger 已使用序号 %d",
					nodeID, declaredActivationSeq[nodeID], derived)
			}
		}
	}
	return transitions, results, activationSeq, nil
}

// validateRecoveredLedger 交叉核对 GraphDocument 当前可见 execution 与独立
// Result/Transition ledger。尤其 terminal Graph 不会再 Resume，若此处放过
// dangling Settlement.ResultRef，损坏将永久潜伏在“已完成”状态中。
func validateRecoveredLedger(doc *GraphDocument, transitions map[transitionKey]TransitionRecord, results map[string]ActivationResult) error {
	if doc == nil {
		return fmt.Errorf("缺少 GraphDocument")
	}
	for _, nodeID := range sortedNodeIDs(doc) {
		node := doc.Nodes[nodeID]
		if node.Execution == nil {
			continue
		}
		if err := validateExecutionLedgerBindings(doc.GraphID, nodeID, *node.Execution, transitions, results, true); err != nil {
			return fmt.Errorf("节点 %s: %w", nodeID, err)
		}
	}
	return nil
}

func validateExecutionLedgerBindings(graphID, nodeID string, exec Execution,
	transitions map[transitionKey]TransitionRecord, results map[string]ActivationResult, allowLegacyInline bool) error {
	if exec.ActivationID != "" {
		owner, _, ok := parseActivationID(exec.ActivationID)
		if !ok || owner != nodeID {
			return fmt.Errorf("execution.activation_id %q owner 不一致", exec.ActivationID)
		}
	}
	if settlement := exec.Settlement; settlement != nil {
		if settlement.ResultRef != "" {
			want := activationResultRef(graphID, exec.ActivationID)
			if settlement.ResultRef != want {
				return fmt.Errorf("settlement.result_ref %q 与稳定引用 %q 不一致", settlement.ResultRef, want)
			}
			stored, ok := results[settlement.ResultRef]
			if !ok || stored.NodeID != nodeID || stored.ActivationID != exec.ActivationID {
				return fmt.Errorf("settlement.result_ref %q 不可解引用或来源不一致", settlement.ResultRef)
			}
			if len(settlement.Result) > 0 && !bytes.Equal(settlement.Result, stored.Result) {
				return fmt.Errorf("settlement legacy result 与 %q 内容不一致", settlement.ResultRef)
			}
		} else if len(settlement.Result) > 0 && allowLegacyInline {
			if err := validateJSONObject(settlement.Result, MaxDocumentBytes, "legacy settlement.result"); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("settlement 缺少可解引用 result_ref（仅 legacy inline result 可兼容）")
		}
	}
	for i, binding := range exec.Input {
		owner, _, ok := parseActivationID(binding.SourceActivationID)
		if !ok || owner != binding.SourceNodeID {
			return fmt.Errorf("input[%d] source_activation_id %q 不属于节点 %q", i, binding.SourceActivationID, binding.SourceNodeID)
		}
		var matched *TransitionRecord
		for _, rec := range transitions {
			if rec.SourceNodeID == binding.SourceNodeID &&
				rec.SourceActivationID == binding.SourceActivationID &&
				rec.TargetNodeID == nodeID && rec.TargetActivationID == exec.ActivationID &&
				rec.TargetInput == binding.TargetInput {
				copy := rec
				matched = &copy
				break
			}
		}
		if matched == nil {
			for _, rec := range transitions {
				if !rec.ReplayInputs || rec.TargetNodeID != nodeID || rec.TargetActivationID != exec.ActivationID {
					continue
				}
				for _, replayed := range rec.ReplayedInputs {
					if reflect.DeepEqual(binding, replayed) {
						copy := rec
						matched = &copy
						break
					}
				}
				if matched != nil {
					break
				}
			}
			if matched == nil {
				return fmt.Errorf("input[%d] 不对应任何同 target activation/port 的 durable transition 或 recovery replay", i)
			}
		}
		if matched.ReplayInputs {
			continue
		}
		if matched.Input == nil || !inputBindingMatchesEdgeInput(binding, *matched.Input) {
			return fmt.Errorf("input[%d] 与对应 durable transition.Input 不一致: binding=%+v transition_input=%+v", i, binding, matched.Input)
		}
		if binding.ResultRef == "" {
			if !allowLegacyInline || len(binding.Result) == 0 {
				return fmt.Errorf("input[%d] 缺少稳定 result_ref", i)
			}
			if err := validateJSONObject(binding.Result, InputInlineMaxBytes, fmt.Sprintf("legacy execution.input[%d].result", i)); err != nil {
				return err
			}
			continue
		}
		want := activationResultRef(graphID, binding.SourceActivationID)
		if binding.ResultRef != want {
			return fmt.Errorf("input[%d].result_ref %q 与来源稳定引用 %q 不一致", i, binding.ResultRef, want)
		}
		stored, ok := results[binding.ResultRef]
		if !ok || stored.NodeID != binding.SourceNodeID || stored.ActivationID != binding.SourceActivationID {
			return fmt.Errorf("input[%d].result_ref %q 不可解引用或来源不一致", i, binding.ResultRef)
		}
		if len(binding.Result) > 0 && !bytes.Equal(binding.Result, stored.Result) {
			return fmt.Errorf("input[%d] 内联 result 与 Result ledger 不一致", i)
		}
		storedEvidence := make(map[string]EvidenceEntry, len(stored.Evidence))
		for _, evidence := range stored.Evidence {
			storedEvidence[evidence.Ref] = evidence
		}
		for _, evidence := range binding.Evidence {
			authoritative, ok := storedEvidence[evidence.Ref]
			if !ok || !sameEvidenceEntry(authoritative, evidence) {
				return fmt.Errorf("input[%d] EvidenceRef %q 与 Result ledger 谱系不一致", i, evidence.Ref)
			}
		}
	}
	return nil
}

func inputBindingMatchesEdgeInput(binding InputBinding, input EdgeInput) bool {
	if binding.Summary != input.Summary || binding.ResultRef != input.ResultRef ||
		binding.Truncated != input.Truncated || !bytes.Equal(binding.Result, input.Result) ||
		len(binding.EvidenceRefs) != len(input.EvidenceRefs) || len(binding.Evidence) != len(input.Evidence) {
		return false
	}
	for i := range binding.EvidenceRefs {
		if binding.EvidenceRefs[i] != input.EvidenceRefs[i] {
			return false
		}
	}
	for i := range binding.Evidence {
		if !sameEvidenceEntry(binding.Evidence[i], input.Evidence[i]) {
			return false
		}
	}
	return true
}

// replayResult 是 journal 重放的结果。
type replayResult struct {
	doc              *GraphDocument // 重放后的文档（无有效条目时为 base，可能 nil）
	seq              int64          // 最后完整条目 seq（无条目则 = base seq）
	lastDigest       string         // 最后完整条目的 digest（无条目为空）
	chainDigest      string         // 最后完整条目的链式 EntryDigest / legacy anchor
	integrityStarted bool           // 已进入 v2 链后不得再降级为 legacy 条目
	applied          int64          // 实际重放的条目数（不含 snapshot 已含的跳过项）
	fileBytes        int64          // 有效前缀字节数（坏行截断后）
	corruptErr       error          // 坏行原因（未发生为 nil）
}

// replayJournal 按 seq 顺序重放 journal 中 seq > baseSeq 的条目。
// 每条条目三重对照：seq 严格连续、revision/state_version 与重算一致、
// digest 与重算一致（含日志尾）；任一不符即按坏行处理：停止重放并把
// 该行及其后物理截断丢弃，以最后一个完整一致状态为准。
// transitions/activationSeq 是 Graph Runtime 的 entry 级簿记底稿（来自
// snapshot），重放中就地叠加：transition 记录进边选择集合，execution 系
// 记录推进 activation 序号。
func replayJournal(path string, base *GraphDocument, baseSeq int64, baseChainDigest string, integrityStarted bool,
	transitions map[transitionKey]TransitionRecord, activationResults map[string]ActivationResult, activationSeq map[string]int) (*replayResult, error) {
	res := &replayResult{doc: base, seq: baseSeq, chainDigest: baseChainDigest, integrityStarted: integrityStarted}
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
		if duplicatePath, key, duplicate := findDuplicateKey(line); duplicate {
			res.corruptErr = fmt.Errorf("偏移 %d 处坏行（重复 JSON key %q，path=%s）", goodLen, key, duplicatePath)
			break
		}
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
		nextChain, nextIntegrity, err := verifyJournalEntryIntegrity(&je, res.chainDigest, res.integrityStarted)
		if err != nil {
			res.corruptErr = fmt.Errorf("seq %d 完整性校验失败: %v", je.Seq, err)
			break
		}
		// applyJournalRecord 会原地修改输入；必须先克隆。否则坏记录即使随后
		// digest/ledger 校验失败，也会污染要保留的最后完整状态。
		candidate := res.doc
		if res.doc != nil {
			candidate, err = cloneDoc(res.doc)
			if err != nil {
				return nil, fmt.Errorf("克隆 journal 重放候选: %w", err)
			}
		}
		next, err := applyJournalRecord(candidate, &je)
		if err != nil {
			res.corruptErr = fmt.Errorf("seq %d 应用失败: %v", je.Seq, err)
			break
		}
		if je.Digest != "" {
			if got := ComputeDigest(next); got != je.Digest {
				res.corruptErr = fmt.Errorf("seq %d digest 不一致：记录 %s，重算 %s", je.Seq, je.Digest, got)
				break
			}
		} else if je.IntegrityVersion != 0 {
			res.corruptErr = fmt.Errorf("seq %d 新格式记录缺少 Graph digest", je.Seq)
			break
		}
		if err := noteJournalBookkeeping(next, &je, transitions, activationResults, activationSeq); err != nil {
			res.corruptErr = fmt.Errorf("seq %d ledger 校验失败: %v", je.Seq, err)
			break
		}
		res.doc = next
		res.seq = je.Seq
		res.lastDigest = je.Digest
		res.chainDigest = nextChain
		res.integrityStarted = nextIntegrity
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

func verifyJournalEntryIntegrity(entry *journalEntry, previous string, integrityStarted bool) (string, bool, error) {
	legacy := entry.IntegrityVersion == 0 && entry.PreviousDigest == "" && entry.EntryDigest == ""
	if legacy {
		if integrityStarted {
			return "", true, fmt.Errorf("已进入链式完整性格式后出现 legacy 降级记录")
		}
		legacyEntry := *entry
		legacyEntry.PreviousDigest = previous
		return hashCanonical(journalDigestInput{
			Domain:           "agentgo.graph.journal/legacy-anchor",
			IntegrityVersion: 0,
			Seq:              legacyEntry.Seq,
			Kind:             legacyEntry.Kind,
			Revision:         legacyEntry.Revision,
			StateVersion:     legacyEntry.StateVersion,
			Digest:           legacyEntry.Digest,
			At:               legacyEntry.At,
			PreviousDigest:   previous,
			Payload:          legacyEntry.Payload,
		}), false, nil
	}
	if entry.IntegrityVersion != journalIntegrityVersion {
		return "", integrityStarted, fmt.Errorf("未知 journal integrity_version=%d", entry.IntegrityVersion)
	}
	if entry.EntryDigest == "" {
		return "", integrityStarted, fmt.Errorf("缺少 entry_digest")
	}
	if entry.PreviousDigest != previous {
		return "", integrityStarted, fmt.Errorf("previous_digest 不连续：记录 %q，期望 %q", entry.PreviousDigest, previous)
	}
	if got := computeJournalEntryDigest(entry); got != entry.EntryDigest {
		return "", integrityStarted, fmt.Errorf("entry_digest 不一致：记录 %s，重算 %s", entry.EntryDigest, got)
	}
	return entry.EntryDigest, true, nil
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
	case journalKindDefinitionAdopted:
		if doc == nil {
			return nil, fmt.Errorf("definition_adopted 记录前缺少 submit")
		}
		var p definitionAdoptPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 definition_adopted payload: %w", err)
		}
		if p.Definition == nil {
			return nil, fmt.Errorf("definition_adopted payload 缺少 definition")
		}
		if err := applyDefinitionAdoption(doc, p.Definition); err != nil {
			return nil, err
		}
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
	case journalKindGraphOutcome:
		if doc == nil {
			return nil, fmt.Errorf("graph_outcome 记录前缺少 submit")
		}
		var p graphOutcomePayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 graph_outcome payload: %w", err)
		}
		status := p.Outcome.Status()
		if status == "" {
			return nil, fmt.Errorf("graph_outcome 含非法 outcome=%q", p.Outcome.Outcome)
		}
		if doc.Outcome != nil {
			return nil, fmt.Errorf("graph_outcome 重复提交：既有 outcome=%q，新 outcome=%q", doc.Outcome.Outcome, p.Outcome.Outcome)
		}
		if !IsValidGraphStatusTransition(doc.Status, status) {
			return nil, fmt.Errorf("graph_outcome 非法状态迁移：%s -> %s", doc.Status, status)
		}
		outcome := p.Outcome
		doc.Outcome = &outcome
		doc.Status = status
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
	case journalKindActivationResult:
		if doc == nil {
			return nil, fmt.Errorf("activation_result 记录前缺少 submit")
		}
		var p activationResultPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return nil, fmt.Errorf("解析 activation_result payload: %w", err)
		}
		// entry 级簿记由 noteJournalBookkeeping 重建；GraphDocument 与
		// state_version 均不变化。
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

// noteJournalBookkeeping 在 entry digest 与 Graph digest 都通过后，校验并叠加
// entry 级账本。校验先于 map 写入，坏记录不得部分污染最后完整前缀。
func noteJournalBookkeeping(doc *GraphDocument, je *journalEntry, transitions map[transitionKey]TransitionRecord, activationResults map[string]ActivationResult, activationSeq map[string]int) error {
	switch je.Kind {
	case journalKindSubmit:
		for id, n := range doc.Nodes {
			if n.Execution != nil {
				if n.Execution.ActivationID != "" {
					owner, _, ok := parseActivationID(n.Execution.ActivationID)
					if !ok || owner != id {
						return fmt.Errorf("submit 节点 %s 的 execution.activation_id %q owner 不一致", id, n.Execution.ActivationID)
					}
				}
				noteActivation(activationSeq, id, n.Execution.ActivationID)
			}
		}
		if err := validateRecoveredLedger(doc, transitions, activationResults); err != nil {
			return err
		}
	case journalKindExecution:
		var p executionPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return fmt.Errorf("解析 execution ledger payload: %w", err)
		}
		if p.Execution.ActivationID != "" {
			owner, _, ok := parseActivationID(p.Execution.ActivationID)
			if !ok || owner != p.NodeID {
				return fmt.Errorf("execution.activation_id %q 不属于节点 %q", p.Execution.ActivationID, p.NodeID)
			}
		}
		if err := validateExecutionLedgerBindings(doc.GraphID, p.NodeID, p.Execution,
			transitions, activationResults, true); err != nil {
			return err
		}
		noteActivation(activationSeq, p.NodeID, p.Execution.ActivationID)
	case journalKindExecutionStatus:
		var p executionStatusPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return fmt.Errorf("解析 execution_status ledger payload: %w", err)
		}
		owner, _, ok := parseActivationID(p.Execution.ActivationID)
		if !ok || owner != p.NodeID {
			return fmt.Errorf("execution_status.activation_id %q 不属于节点 %q", p.Execution.ActivationID, p.NodeID)
		}
		if err := validateExecutionLedgerBindings(doc.GraphID, p.NodeID, p.Execution,
			transitions, activationResults, true); err != nil {
			return err
		}
		noteActivation(activationSeq, p.NodeID, p.Execution.ActivationID)
	case journalKindTransition:
		var p transitionPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return fmt.Errorf("解析 transition ledger payload: %w", err)
		}
		rec := TransitionRecord(p)
		if err := validateTransitionRecord(doc.GraphID, doc, rec, activationResults, je.IntegrityVersion == 0); err != nil {
			return err
		}
		key := transitionKey{p.SourceActivationID, p.TransitionID}
		if previous, exists := transitions[key]; exists {
			if sameTransitionRecord(previous, rec) {
				return nil
			}
			return fmt.Errorf("transition identity (%s,%d) 已存在且内容不一致", p.SourceActivationID, p.TransitionID)
		}
		transitions[key] = cloneTransitionRecord(rec)
		noteActivation(activationSeq, rec.TargetNodeID, rec.TargetActivationID)
	case journalKindActivationResult:
		var p activationResultPayload
		if err := json.Unmarshal(je.Payload, &p); err != nil {
			return fmt.Errorf("解析 activation_result ledger payload: %w", err)
		}
		rec := ActivationResult(p)
		if err := validateActivationResultRecord(doc.GraphID, doc, rec, true); err != nil {
			return err
		}
		if err := validateActivationResultAgainstLedger(rec, activationResults); err != nil {
			return err
		}
		if previous, exists := activationResults[rec.Ref]; exists && sameActivationResult(previous, rec) {
			return nil
		}
		activationResults[rec.Ref] = cloneActivationResult(rec)
		noteActivation(activationSeq, rec.NodeID, rec.ActivationID)
	}
	return nil
}
