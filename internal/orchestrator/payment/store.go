package payment

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Record struct {
	Proof          Proof     `json:"proof"`
	AcknowledgedAt time.Time `json:"acknowledged_at,omitempty"`
}

type Store interface {
	Put(Proof) (bool, error)
	Acknowledge(string, time.Time) error
	RecordForTask(string, string, string) (Record, bool)
	Pending() []Proof
	Records() []Record
}

func (s *FileStore) RecordForTask(workerID, taskID, offerID string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, record := range s.records {
		proof := record.Proof
		if proof.WorkerID == workerID && proof.TaskID == taskID && proof.OfferID == offerID {
			return record, true
		}
	}
	return Record{}, false
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
		return nil, errors.New("payment evidence state path is required")
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
	if err := json.Unmarshal(encoded, &state); err != nil || state.Version != 1 {
		return nil, errors.New("invalid payment evidence state")
	}
	for id, record := range state.Records {
		if id != record.Proof.EvidenceID || record.Proof.Verify() != nil {
			return nil, errors.New("invalid persisted payment evidence")
		}
		store.records[id] = record
	}
	return store, nil
}

func (s *FileStore) Put(proof Proof) (bool, error) {
	if err := proof.Verify(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[proof.EvidenceID]; exists {
		return false, nil
	}
	s.records[proof.EvidenceID] = Record{Proof: proof}
	if err := s.persistLocked(); err != nil {
		delete(s.records, proof.EvidenceID)
		return false, err
	}
	return true, nil
}

func (s *FileStore) Acknowledge(id string, at time.Time) error {
	if at.IsZero() {
		return errors.New("payment evidence acknowledgement time is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[id]
	if !exists {
		return errors.New("payment evidence not found")
	}
	if !record.AcknowledgedAt.IsZero() {
		return nil
	}
	record.AcknowledgedAt = at.UTC()
	s.records[id] = record
	if err := s.persistLocked(); err != nil {
		record.AcknowledgedAt = time.Time{}
		s.records[id] = record
		return err
	}
	return nil
}

func (s *FileStore) Pending() []Proof {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Proof
	for _, record := range s.records {
		if record.AcknowledgedAt.IsZero() {
			result = append(result, record.Proof)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].EvidenceID < result[j].EvidenceID })
	return result
}

func (s *FileStore) Records() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Record, 0, len(s.records))
	for _, record := range s.records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Proof.EvidenceID < result[j].Proof.EvidenceID })
	return result
}

func (s *FileStore) persistLocked() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".payments-*.tmp")
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
	return os.Rename(temporary, s.path)
}

var _ Store = (*FileStore)(nil)
