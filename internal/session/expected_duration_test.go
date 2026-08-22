package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTaskSnapshotExpectedDurationJSONCompatibility(t *testing.T) {
	want := TaskSnapshot{ID: "task-1", ExpectedDuration: 45 * time.Second, TimeoutSeconds: 9}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got TaskSnapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExpectedDuration != want.ExpectedDuration || got.TimeoutSeconds != want.TimeoutSeconds {
		t.Fatalf("快照往返不一致: got=%+v want=%+v", got, want)
	}

	// 旧 JSON 没有 expected_duration，必须继续保留 timeout_seconds 供 Store
	// 导入边界迁移，session 层不擅自改变其单位或语义。
	got = TaskSnapshot{}
	if err := json.Unmarshal([]byte(`{"id":"legacy","timeout_seconds":12}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.ExpectedDuration != 0 || got.TimeoutSeconds != 12 {
		t.Fatalf("旧 JSON 兼容失败: %+v", got)
	}
}
