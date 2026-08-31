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

// Store 以每个 Delivery 一份原子 JSON 文件持久化 Transaction。它不重放
// workspace 副作用；只记录候选、验收与 promotion 的确定状态。
type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("DeliveryStore 目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) EnsureOpen(tx Transaction) (Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok, err := s.getLocked(tx.ID); err != nil {
		return Transaction{}, err
	} else if ok {
		if existing.RunID != tx.RunID || existing.GraphID != tx.GraphID ||
			existing.ProducerActivationID != tx.ProducerActivationID {
			return Transaction{}, fmt.Errorf("Delivery %s identity 冲突", tx.ID)
		}
		return existing, nil
	}
	if tx.Schema == "" {
		tx.Schema = SchemaV1
	}
	if tx.Status == "" {
		tx.Status = StatusOpen
	}
	if tx.UpdatedAt.IsZero() {
		tx.UpdatedAt = time.Now().UTC()
	}
	if err := tx.Validate(); err != nil {
		return Transaction{}, err
	}
	return tx, s.writeLocked(tx)
}

func (s *Store) Get(id string) (Transaction, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

func (s *Store) PrepareCandidate(id string, candidate Candidate, fulfillmentRef string,
	evidenceRefs []string, producerOutcomeRef string, now time.Time,
) (Transaction, error) {
	return s.update(id, func(tx Transaction) (Transaction, error) {
		if tx.Status == StatusPrepared && tx.Candidate != nil && tx.Candidate.Ref == candidate.Ref {
			return tx, nil
		}
		var err error
		switch tx.Status {
		case StatusOpen, StatusRepairing:
			tx.Candidate = &candidate
			tx.FulfillmentRef = fulfillmentRef
			tx.EvidenceRefs = append([]string(nil), evidenceRefs...)
			tx.ProducerOutcomeRef = producerOutcomeRef
			tx, err = tx.Transition(StatusPrepared, now)
		default:
			return Transaction{}, fmt.Errorf("Delivery %s status=%s 不能冻结 candidate", id, tx.Status)
		}
		return tx, err
	})
}

func (s *Store) BeginVerification(id string, now time.Time) (Transaction, error) {
	return s.transition(id, StatusVerifying, now, nil)
}

func (s *Store) BeginRepair(id string, now time.Time) (Transaction, error) {
	return s.transition(id, StatusRepairing, now, nil)
}

func (s *Store) PrepareCommit(id, acceptanceOutcomeRef, intentRef string, now time.Time) (Transaction, error) {
	return s.transition(id, StatusCommitPrepared, now, func(tx *Transaction) {
		tx.AcceptanceOutcomeRef = acceptanceOutcomeRef
		tx.CommitIntentRef = intentRef
	})
}

func (s *Store) Commit(id, effectRef, revisionRef string, now time.Time) (Transaction, error) {
	return s.transition(id, StatusCommitted, now, func(tx *Transaction) {
		tx.CommitEffectRef = effectRef
		tx.CommittedRevisionRef = revisionRef
	})
}

func (s *Store) CommitUnknown(id, effectRef string, now time.Time) (Transaction, error) {
	return s.transition(id, StatusCommitUnknown, now, func(tx *Transaction) {
		tx.CommitEffectRef = effectRef
	})
}

func (s *Store) Quarantine(id, reason string, now time.Time) (Transaction, error) {
	return s.transition(id, StatusQuarantined, now, func(tx *Transaction) {
		tx.QuarantineReason = strings.TrimSpace(reason)
	})
}

func (s *Store) transition(id string, to Status, now time.Time, mutate func(*Transaction)) (Transaction, error) {
	return s.update(id, func(tx Transaction) (Transaction, error) {
		if tx.Status == to {
			return tx, nil
		}
		if mutate != nil {
			mutate(&tx)
		}
		return tx.Transition(to, now)
	})
}

func (s *Store) update(id string, fn func(Transaction) (Transaction, error)) (Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok, err := s.getLocked(id)
	if err != nil {
		return Transaction{}, err
	}
	if !ok {
		return Transaction{}, fmt.Errorf("Delivery %s 不存在", id)
	}
	next, err := fn(tx)
	if err != nil {
		return Transaction{}, err
	}
	if err := next.Validate(); err != nil {
		return Transaction{}, err
	}
	return next, s.writeLocked(next)
}

func (s *Store) List() ([]Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Transaction
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var tx Transaction
		if err := json.Unmarshal(data, &tx); err != nil {
			return nil, err
		}
		if err := tx.Validate(); err != nil {
			return nil, err
		}
		out = append(out, tx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) getLocked(id string) (Transaction, bool, error) {
	if strings.TrimSpace(id) == "" {
		return Transaction{}, false, fmt.Errorf("Delivery ID 不能为空")
	}
	data, err := os.ReadFile(s.path(id))
	if os.IsNotExist(err) {
		return Transaction{}, false, nil
	}
	if err != nil {
		return Transaction{}, false, err
	}
	var tx Transaction
	if err := json.Unmarshal(data, &tx); err != nil {
		return Transaction{}, false, err
	}
	if tx.ID != id {
		return Transaction{}, false, fmt.Errorf("Delivery 文件 identity 不一致")
	}
	if err := tx.Validate(); err != nil {
		return Transaction{}, false, err
	}
	return tx, true, nil
}

func (s *Store) writeLocked(tx Transaction) error {
	encoded, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".delivery-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if _, err = tmp.Write(encoded); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		cleanup()
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, s.path(tx.ID)); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (s *Store) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}
