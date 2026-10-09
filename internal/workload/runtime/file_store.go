package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

const fileStoreVersion = 1
const maxRetainedReceiptCommittedWorkloads = 256
const workloadCompactionOperations = 256

type workloadFrame struct {
	Record Record `json:"record"`
}

type fileStoreSnapshot struct {
	Version int               `json:"version"`
	Records map[string]Record `json:"records"`
}

type FileStore struct {
	mu         sync.RWMutex
	path       string
	records    map[string]Record
	operations int
	compacting bool
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
		return store, store.replayJournal()
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
	return store, store.replayJournal()
}

func (s *FileStore) Create(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.Spec.Key()
	if _, exists := s.records[key]; exists {
		return ErrRecordExists
	}
	s.records[key] = cloneRecord(record)
	if err := s.appendLocked(record); err != nil {
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
	if err := s.appendLocked(record); err != nil {
		s.records[key] = previous
		return err
	}
	return nil
}

func (s *FileStore) replayJournal() error {
	data, err := os.ReadFile(s.path + ".wal")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		end := bytes.LastIndexByte(data, '\n') + 1
		data = data[:end]
		if err := os.Truncate(s.path+".wal", int64(end)); err != nil {
			return err
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	for scanner.Scan() {
		var frame workloadFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return fmt.Errorf("decode workload journal: %w", err)
		}
		if frame.Record.Spec.WorkloadID == "" || frame.Record.Spec.AttemptID == "" {
			return errors.New("workload journal identity is missing")
		}
		s.records[frame.Record.Spec.Key()] = frame.Record
		s.operations++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	s.records = compactReceiptCommittedWorkloads(s.records, maxRetainedReceiptCommittedWorkloads)
	return nil
}

func (s *FileStore) appendLocked(record Record) error {
	path := s.path + ".wal"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if err = json.NewEncoder(file).Encode(workloadFrame{Record: record}); err == nil {
		err = file.Sync()
	}
	if err != nil {
		_ = file.Truncate(info.Size())
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if info.Size() == 0 {
		handle, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = handle.Sync()
		_ = handle.Close()
		if err != nil {
			return err
		}
	}
	s.operations++
	s.records = compactReceiptCommittedWorkloads(s.records, maxRetainedReceiptCommittedWorkloads)
	if s.operations >= workloadCompactionOperations && !s.compacting {
		s.startCompactionLocked()
	}
	return nil
}

func (s *FileStore) startCompactionLocked() {
	info, err := os.Stat(s.path + ".wal")
	if err != nil {
		return
	}
	s.compacting = true
	offset := info.Size()
	operations := s.operations
	records := maps.Clone(s.records)
	go func() {
		temporary, err := prepareWorkloadSnapshot(s.path, records)
		s.mu.Lock()
		defer s.mu.Unlock()
		defer func() { s.compacting = false }()
		if err != nil {
			return
		}
		defer os.Remove(temporary)
		if err := replaceWorkloadSnapshot(temporary, s.path); err != nil {
			return
		}
		if err := compactWorkloadJournal(s.path+".wal", offset); err != nil {
			return
		}
		s.operations -= operations
	}()
}

func compactWorkloadJournal(path string, offset int64) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	if _, err := source.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".workloads-wal-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	_, err = io.Copy(temporary, source)
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	_ = source.Close()
	return replaceWorkloadSnapshot(temporary.Name(), path)
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
	compacted := compactReceiptCommittedWorkloads(s.records, maxRetainedReceiptCommittedWorkloads)
	temporary, err := prepareWorkloadSnapshot(s.path, compacted)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := replaceWorkloadSnapshot(temporary, s.path); err != nil {
		return err
	}
	s.records = compacted
	return nil
}

func prepareWorkloadSnapshot(path string, records map[string]Record) (string, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(directory, ".workloads-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	fail := func(err error) (string, error) { _ = temporary.Close(); _ = os.Remove(temporaryPath); return "", err }
	if err := temporary.Chmod(0o600); err != nil {
		return fail(err)
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(fileStoreSnapshot{Version: fileStoreVersion, Records: records}); err != nil {
		return fail(err)
	}
	if err := temporary.Sync(); err != nil {
		return fail(err)
	}
	if err := temporary.Close(); err != nil {
		return fail(err)
	}
	return temporaryPath, nil
}

func replaceWorkloadSnapshot(temporaryPath, path string) error {
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return err
}

func compactReceiptCommittedWorkloads(records map[string]Record, limit int) map[string]Record {
	if limit < 0 {
		limit = 0
	}
	terminal := make([]string, 0)
	for key, record := range records {
		if record.State == domain.StateReceiptCommitted {
			terminal = append(terminal, key)
		}
	}
	if len(terminal) <= limit {
		return records
	}
	sort.Slice(terminal, func(i, j int) bool {
		left, right := records[terminal[i]], records[terminal[j]]
		if left.UpdatedAt.Equal(right.UpdatedAt) {
			return terminal[i] > terminal[j]
		}
		return left.UpdatedAt.After(right.UpdatedAt)
	})
	compacted := make(map[string]Record, len(records)-(len(terminal)-limit))
	for key, record := range records {
		compacted[key] = record
	}
	for _, key := range terminal[limit:] {
		delete(compacted, key)
	}
	return compacted
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
