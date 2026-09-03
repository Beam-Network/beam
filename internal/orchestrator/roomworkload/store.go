package roomworkload

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type MemoryStore[D, U, I, C, P, R any] struct {
	mu      sync.RWMutex
	records map[string]Record[D, U, I, C, P, R]
}

func NewMemoryStore[D, U, I, C, P, R any]() *MemoryStore[D, U, I, C, P, R] {
	return &MemoryStore[D, U, I, C, P, R]{records: make(map[string]Record[D, U, I, C, P, R])}
}

func (s *MemoryStore[D, U, I, C, P, R]) Get(key string) (Record[D, U, I, C, P, R], bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.records[key]
	return cloneRecord(value), ok
}

func (s *MemoryStore[D, U, I, C, P, R]) Put(value Record[D, U, I, C, P, R]) error {
	if value.Definition.ID == "" {
		return errors.New("room workload definition id is required")
	}
	s.mu.Lock()
	s.records[value.Definition.ID] = cloneRecord(value)
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore[D, U, I, C, P, R]) List() []Record[D, U, I, C, P, R] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedRecords(s.records)
}

type FileStore[D, U, I, C, P, R any] struct {
	mu      sync.RWMutex
	path    string
	records map[string]Record[D, U, I, C, P, R]
}

type fileSnapshot[D, U, I, C, P, R any] struct {
	Version int                                 `json:"version"`
	Records map[string]Record[D, U, I, C, P, R] `json:"records"`
}

func OpenFileStore[D, U, I, C, P, R any](path string) (*FileStore[D, U, I, C, P, R], error) {
	if path == "" {
		return nil, errors.New("room workload state path is required")
	}
	store := &FileStore[D, U, I, C, P, R]{path: path, records: make(map[string]Record[D, U, I, C, P, R])}
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
	var snapshot fileSnapshot[D, U, I, C, P, R]
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.Version != 1 {
		return nil, errors.New("unsupported room workload state version")
	}
	if snapshot.Records != nil {
		store.records = snapshot.Records
	}
	return store, nil
}

func (s *FileStore[D, U, I, C, P, R]) Get(key string) (Record[D, U, I, C, P, R], bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.records[key]
	return cloneRecord(value), ok
}

func (s *FileStore[D, U, I, C, P, R]) Put(value Record[D, U, I, C, P, R]) error {
	if value.Definition.ID == "" {
		return errors.New("room workload definition id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.records[value.Definition.ID]
	s.records[value.Definition.ID] = cloneRecord(value)
	if err := s.persist(); err != nil {
		if existed {
			s.records[value.Definition.ID] = previous
		} else {
			delete(s.records, value.Definition.ID)
		}
		return err
	}
	return nil
}

func (s *FileStore[D, U, I, C, P, R]) List() []Record[D, U, I, C, P, R] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedRecords(s.records)
}

func (s *FileStore[D, U, I, C, P, R]) persist() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".room-workloads-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(fileSnapshot[D, U, I, C, P, R]{Version: 1, Records: s.records}); err != nil {
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

func cloneRecord[D, U, I, C, P, R any](value Record[D, U, I, C, P, R]) Record[D, U, I, C, P, R] {
	encoded, _ := json.Marshal(value)
	var result Record[D, U, I, C, P, R]
	_ = json.Unmarshal(encoded, &result)
	return result
}

func sortedRecords[D, U, I, C, P, R any](values map[string]Record[D, U, I, C, P, R]) []Record[D, U, I, C, P, R] {
	result := make([]Record[D, U, I, C, P, R], 0, len(values))
	for _, value := range values {
		result = append(result, cloneRecord(value))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}
