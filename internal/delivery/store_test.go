package delivery

import (
	"os"
	"testing"
)

func TestCommitIdentityAndTerminalImmutability(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value := CommitRecord{ID: "delivery:1", RunID: "run", GraphID: "graph", CompletionRef: "completion", CandidateRef: "candidate", EffectRef: "effect"}
	prepared, err := s.Prepare(value)
	if err != nil || prepared.Status != "prepared" {
		t.Fatal(err)
	}
	repeat, err := s.Prepare(value)
	if err != nil || !repeat.UpdatedAt.Equal(prepared.UpdatedAt) {
		t.Fatal("重复请求改写交付意图", err)
	}
	value.CandidateRef = "other"
	if _, err = s.Prepare(value); err == nil {
		t.Fatal("同一交付 ID 不能换候选")
	}
	final, err := s.Finish("delivery:1", true, "")
	if err != nil || final.Status != "committed" {
		t.Fatal(err)
	}
	if _, err = s.Finish("delivery:1", false, "不得降级"); err == nil {
		t.Fatal("终态不可改写")
	}
	reopened, err := NewStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := reopened.Get("delivery:1")
	if err != nil || !ok || got.EffectRef != "effect" || got.Status != "committed" {
		t.Fatal("恢复丢失实际提交身份")
	}
}
func TestDeliveryRejectsOldSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.path("old"), []byte(`{"schema":"agentgo.delivery/v1","delivery_id":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewStore(dir); err == nil {
		t.Fatal("不得回退旧验收交付契约")
	}
}
