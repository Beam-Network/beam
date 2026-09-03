package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const fileStoreVersion = 1

type fileStoreSnapshot struct {
	Version int               `json:"version"`
	Records map[string]Record `json:"records"`
}

// FileStore is an owner-local durable workload journal. Every mutation is
// written to a temporary file, fsynced, and atomically renamed over the
// previous snapshot.
type FileStore struct {
	mu      sync.RWMutex
	path    string
	records map[string]Record
}

func OpenFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("workload store path is required")
	}
	store := &FileStore{path: path, records: make(map[string]Record)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot fileStoreSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("decode workload store: %w", err)
	}
	if snapshot.Version != fileStoreVersion {
		return nil, fmt.Errorf("unsupported workload store version %d", snapshot.Version)
	}
	if snapshot.Records != nil {
		store.records = snapshot.Records
	}
	return store, nil
}

func (s *FileStore) Create(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.Spec.Key()
	if _, exists := s.records[key]; exists {
		return ErrRecordExists
	}
	s.records[key] = cloneRecord(record)
	if err := s.persistLocked(); err != nil {
		delete(s.records, key)
		return err
	}
	return nil
}

func (s *FileStore) Get(key string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, exists := s.records[key]
	if !exists {
		return Record{}, ErrRecordNotFound
	}
	return cloneRecord(record), nil
}

func (s *FileStore) Save(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.Spec.Key()
	previous, exists := s.records[key]
	if !exists {
		return ErrRecordNotFound
	}
	s.records[key] = cloneRecord(record)
	if err := s.persistLocked(); err != nil {
		s.records[key] = previous
		return err
	}
	return nil
}

func (s *FileStore) List() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]Record, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, cloneRecord(record))
	}
	return records
}

func (s *FileStore) persistLocked() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".workloads-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(fileStoreSnapshot{Version: fileStoreVersion, Records: s.records}); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err == nil {
		err = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return err
}

func cloneRecord(record Record) Record {
	cloned := record
	cloned.Spec.RequiredCapabilities = slices.Clone(record.Spec.RequiredCapabilities)
	cloned.Spec.Security.NetworkTargets = slices.Clone(record.Spec.Security.NetworkTargets)
	cloned.Spec.Security.Permissions = slices.Clone(record.Spec.Security.Permissions)
	cloned.Spec.Evidence.Commitments = slices.Clone(record.Spec.Evidence.Commitments)
	cloned.Spec.Payload = bytes.Clone(record.Spec.Payload)
	if record.Result != nil {
		result := *record.Result
		result.Outputs = maps.Clone(record.Result.Outputs)
		cloned.Result = &result
	}
	if record.Progress != nil {
		progress := *record.Progress
		progress.Outputs = maps.Clone(record.Progress.Outputs)
		cloned.Progress = &progress
	}
	if record.Checkpoint != nil {
		checkpoint := *record.Checkpoint
		checkpoint.Cursor = maps.Clone(record.Checkpoint.Cursor)
		checkpoint.Payload = bytes.Clone(record.Checkpoint.Payload)
		cloned.Checkpoint = &checkpoint
	}
	return cloned
}

var _ Store = (*FileStore)(nil)
