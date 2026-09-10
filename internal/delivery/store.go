// Package delivery 记录图级文件提交，不参与任务种类或验收决策。
package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const SchemaCurrent = "agentgo.delivery/v2"

type CommitRecord struct {
	Schema        string    `json:"schema"`
	ID            string    `json:"delivery_id"`
	RunID         string    `json:"run_id"`
	GraphID       string    `json:"graph_id"`
	CompletionRef string    `json:"completion_ref"`
	CandidateRef  string    `json:"candidate_ref"`
	EffectRef     string    `json:"effect_ref"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (c CommitRecord) Validate() error {
	if c.Schema != SchemaCurrent {
		return fmt.Errorf("旧 Delivery 契约不受支持")
	}
	for _, s := range []string{c.ID, c.RunID, c.GraphID, c.CompletionRef, c.CandidateRef, c.EffectRef} {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("交付身份缺失")
		}
	}
	if c.Status != "prepared" && c.Status != "committed" && c.Status != "unknown" {
		return fmt.Errorf("非法交付状态")
	}
	if c.UpdatedAt.IsZero() {
		return fmt.Errorf("交付时间缺失")
	}
	return nil
}

type Store struct {
	mu  sync.Mutex
	dir string
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("交付目录为空")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if _, err := s.List(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}
func (s *Store) get(id string) (CommitRecord, bool, error) {
	var value CommitRecord
	data, err := os.ReadFile(s.path(id))
	if os.IsNotExist(err) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	if err = json.Unmarshal(data, &value); err != nil {
		return value, false, err
	}
	if err = value.Validate(); err != nil {
		return value, false, err
	}
	if value.ID != id {
		return value, false, fmt.Errorf("交付文件身份不符")
	}
	return value, true, nil
}
func (s *Store) Get(id string) (CommitRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}
func (s *Store) Prepare(value CommitRecord) (CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok, err := s.get(value.ID); err != nil {
		return old, err
	} else if ok {
		if old.CandidateRef != value.CandidateRef || old.CompletionRef != value.CompletionRef || old.GraphID != value.GraphID || old.RunID != value.RunID || old.EffectRef != value.EffectRef {
			return old, fmt.Errorf("交付身份冲突")
		}
		return old, nil
	}
	value.Schema = SchemaCurrent
	value.Status = "prepared"
	value.UpdatedAt = time.Now().UTC()
	return value, s.write(value)
}
func (s *Store) Finish(id string, success bool, reason string) (CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok, err := s.get(id)
	if err != nil || !ok {
		return value, fmt.Errorf("没有交付意图: %v", err)
	}
	status := "unknown"
	if success {
		status = "committed"
	}
	if value.Status == status {
		return value, nil
	}
	if value.Status != "prepared" {
		return value, fmt.Errorf("已结算交付不可改写")
	}
	value.Status = status
	value.Error = reason
	value.UpdatedAt = time.Now().UTC()
	return value, s.write(value)
}
func (s *Store) write(value CommitRecord) error {
	if err := value.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".delivery-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, s.path(value.ID))
}
func (s *Store) List() ([]CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []CommitRecord
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var value CommitRecord
		if err = json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		if err = value.Validate(); err != nil {
			return nil, err
		}
		if s.path(value.ID) != p {
			return nil, fmt.Errorf("交付文件身份不符")
		}
		out = append(out, value)
	}
	return out, nil
}
