package runtime

import (
	"errors"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

var (
	ErrRecordExists   = errors.New("workload record already exists")
	ErrRecordNotFound = errors.New("workload record not found")
)

type Record struct {
	Spec        domain.Spec
	State       domain.State
	Reason      string
	PlanVersion uint64
	Result      *domain.Result
	Progress    *domain.Progress
	Checkpoint  *domain.Checkpoint
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Store interface {
	Create(Record) error
	Get(string) (Record, error)
	Save(Record) error
	List() []Record
}

type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]Record)}
}

func (s *MemoryStore) Create(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.Spec.Key()
	if _, exists := s.records[key]; exists {
		return ErrRecordExists
	}
	s.records[key] = record
	return nil
}

func (s *MemoryStore) Get(key string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.records[key]
	if !ok {
		return Record{}, ErrRecordNotFound
	}
	return record, nil
}

func (s *MemoryStore) Save(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.Spec.Key()
	if _, exists := s.records[key]; !exists {
		return ErrRecordNotFound
	}
	s.records[key] = record
	return nil
}

func (s *MemoryStore) List() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]Record, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	return records
}
