package dispatch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Store interface {
	Get(string) (Record, bool)
	Put(Record) error
	List() []Record
}

type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{records: make(map[string]Record)} }

func (s *MemoryStore) Get(key string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.records[key]
	return cloneRecord(record), ok
}

func (s *MemoryStore) Put(record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.records[record.WorkloadKey] = cloneRecord(record)
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) List() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedRecords(s.records)
}

type FileStore struct {
	mu      sync.RWMutex
	path    string
	records map[string]Record
}

type snapshot struct {
	Version int               `json:"version"`
	Records map[string]Record `json:"records"`
}

func OpenFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("Orchestrator orchestration state path is required")
	}
	store := &FileStore{path: path, records: make(map[string]Record)}
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var state snapshot
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, err
	}
	if state.Version != 1 {
		return nil, errors.New("unsupported Orchestrator orchestration state version")
	}
	if state.Records != nil {
		for key, record := range state.Records {
			if key != record.WorkloadKey {
				return nil, errors.New("Orchestrator orchestration snapshot key mismatch")
			}
			if err := record.Validate(); err != nil {
				return nil, err
			}
		}
		store.records = state.Records
	}
	return store, nil
}

func (s *FileStore) Get(key string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.records[key]
	return cloneRecord(record), ok
}

func (s *FileStore) Put(record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.records[record.WorkloadKey]
	s.records[record.WorkloadKey] = cloneRecord(record)
	if err := s.persistLocked(); err != nil {
		if existed {
			s.records[record.WorkloadKey] = previous
		} else {
			delete(s.records, record.WorkloadKey)
		}
		return err
	}
	return nil
}

func (s *FileStore) List() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedRecords(s.records)
}

func (s *FileStore) persistLocked() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".orchestrator-tasks-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(snapshot{Version: 1, Records: s.records}); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return err
	}
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func sortedRecords(records map[string]Record) []Record {
	result := make([]Record, 0, len(records))
	for _, record := range records {
		result = append(result, cloneRecord(record))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func cloneRecord(record Record) Record {
	encoded, _ := json.Marshal(record)
	var cloned Record
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

var _ Store = (*MemoryStore)(nil)
var _ Store = (*FileStore)(nil)
