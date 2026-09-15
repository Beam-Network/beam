package roomtransfer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

type LaneState string

const (
	LanePending     LaneState = "pending"
	LaneProvisioned LaneState = "provisioned"
	LaneDispatched  LaneState = "dispatched"
	LaneCompleted   LaneState = "completed"
	LaneFailed      LaneState = "failed"
	LaneCancelled   LaneState = "cancelled"
)

type LaneRecord struct {
	SourceReads             []contracts.SourceReadEvidence       `json:"source_reads,omitempty"`
	StorageResults          []contracts.StorageRangeResult       `json:"storage_results,omitempty"`
	LaneID                  string                               `json:"lane_id"`
	WorkerID                string                               `json:"worker_id,omitempty"`
	NodeID                  string                               `json:"node_id,omitempty"`
	State                   LaneState                            `json:"state"`
	SourceLease             *contracts.TunnelLease               `json:"source_lease,omitempty"`
	TargetLeases            map[string]contracts.TunnelLease     `json:"target_leases,omitempty"`
	ProvisionFailures       map[string]contracts.RoomFailure     `json:"provision_failures,omitempty"`
	WorkloadKey             string                               `json:"workload_key,omitempty"`
	SourceReceipts          []contracts.SourceRangeReceipt       `json:"source_receipts,omitempty"`
	TargetReceipts          []contracts.TargetRangeReceipt       `json:"target_receipts,omitempty"`
	FinalTargetReceipts     []contracts.FinalTargetReceipt       `json:"final_target_receipts,omitempty"`
	WorkerAcknowledgedAt    *time.Time                           `json:"worker_acknowledged_at,omitempty"`
	ExecutableLeaseIssuedAt *time.Time                           `json:"executable_lease_issued_at,omitempty"`
	Runtime                 *contracts.DirectRoomTransferRuntime `json:"runtime,omitempty"`
	ReportedResultIDs       map[string]bool                      `json:"reported_result_ids,omitempty"`
	Error                   string                               `json:"error,omitempty"`
}

type Record struct {
	Batch     contracts.RoomTaskOfferBatch `json:"batch"`
	Lanes     map[string]LaneRecord        `json:"lanes"`
	CreatedAt time.Time                    `json:"created_at"`
	UpdatedAt time.Time                    `json:"updated_at"`
}

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
	value, ok := s.records[key]
	return clone(value), ok
}
func (s *MemoryStore) Put(value Record) error {
	if value.Batch.BatchID == "" {
		return errors.New("room transfer batch id is required")
	}
	s.mu.Lock()
	s.records[value.Batch.BatchID] = clone(value)
	s.mu.Unlock()
	return nil
}
func (s *MemoryStore) List() []Record { s.mu.RLock(); defer s.mu.RUnlock(); return sorted(s.records) }

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
		return nil, errors.New("room transfer state path is required")
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
		return nil, errors.New("unsupported room transfer state version")
	}
	if state.Records != nil {
		store.records = state.Records
	}
	return store, nil
}
func (s *FileStore) Get(key string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.records[key]
	return clone(value), ok
}
func (s *FileStore) List() []Record { s.mu.RLock(); defer s.mu.RUnlock(); return sorted(s.records) }
func (s *FileStore) Put(value Record) error {
	if value.Batch.BatchID == "" {
		return errors.New("room transfer batch id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.records[value.Batch.BatchID]
	s.records[value.Batch.BatchID] = clone(value)
	if err := s.persist(); err != nil {
		if existed {
			s.records[value.Batch.BatchID] = previous
		} else {
			delete(s.records, value.Batch.BatchID)
		}
		return err
	}
	return nil
}
func (s *FileStore) persist() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".room-transfers-*.tmp")
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
func clone(value Record) Record {
	encoded, _ := json.Marshal(value)
	var result Record
	_ = json.Unmarshal(encoded, &result)
	return result
}
func sorted(values map[string]Record) []Record {
	result := make([]Record, 0, len(values))
	for _, value := range values {
		result = append(result, clone(value))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

var _ Store = (*MemoryStore)(nil)
var _ Store = (*FileStore)(nil)
