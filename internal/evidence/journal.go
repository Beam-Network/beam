package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const journalVersion = 1
const maxRetainedAcknowledgedReceipts = 256

var ErrReceiptNotFound = errors.New("receipt not found")

type JournalRecord struct {
	Receipt        Receipt   `json:"receipt"`
	AcknowledgedAt time.Time `json:"acknowledged_at,omitempty"`
}

type Journal interface {
	Put(Receipt) (bool, error)
	Acknowledge(string, time.Time) error
	Get(string) (JournalRecord, error)
	Pending() []Receipt
	Records() []JournalRecord
}

type memoryJournal struct {
	mu      sync.RWMutex
	records map[string]JournalRecord
}

func NewMemoryJournal() Journal {
	return &memoryJournal{records: make(map[string]JournalRecord)}
}

func (j *memoryJournal) Put(receipt Receipt) (bool, error) {
	if err := receipt.Verify(); err != nil {
		return false, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if existing, ok := j.records[receipt.ReceiptID]; ok {
		if !sameReceipt(existing.Receipt, receipt) {
			return false, errors.New("receipt id conflicts with existing journal record")
		}
		return false, nil
	}
	j.records[receipt.ReceiptID] = JournalRecord{Receipt: receipt}
	return true, nil
}

func (j *memoryJournal) Acknowledge(receiptID string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.records[receiptID]
	if !ok {
		return ErrReceiptNotFound
	}
	if record.AcknowledgedAt.IsZero() {
		record.AcknowledgedAt = at.UTC()
		j.records[receiptID] = record
	}
	return nil
}

func (j *memoryJournal) Get(receiptID string) (JournalRecord, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	record, ok := j.records[receiptID]
	if !ok {
		return JournalRecord{}, ErrReceiptNotFound
	}
	return cloneJournalRecord(record), nil
}

func (j *memoryJournal) Pending() []Receipt {
	j.mu.RLock()
	defer j.mu.RUnlock()
	var receipts []Receipt
	for _, record := range j.records {
		if record.AcknowledgedAt.IsZero() {
			receipts = append(receipts, record.Receipt)
		}
	}
	sortReceipts(receipts)
	return receipts
}

func (j *memoryJournal) Records() []JournalRecord {
	j.mu.RLock()
	defer j.mu.RUnlock()
	records := make([]JournalRecord, 0, len(j.records))
	for _, record := range j.records {
		records = append(records, cloneJournalRecord(record))
	}
	sort.Slice(records, func(i, k int) bool { return records[i].Receipt.ReceiptID < records[k].Receipt.ReceiptID })
	return records
}

type fileSnapshot struct {
	Version int                      `json:"version"`
	Records map[string]JournalRecord `json:"records"`
}

type FileJournal struct {
	mu      sync.RWMutex
	path    string
	records map[string]JournalRecord
}

func OpenFileJournal(path string) (*FileJournal, error) {
	if path == "" {
		return nil, errors.New("receipt journal path is required")
	}
	journal := &FileJournal{path: path, records: make(map[string]JournalRecord)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return journal, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot fileSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("decode receipt journal: %w", err)
	}
	if snapshot.Version != journalVersion {
		return nil, fmt.Errorf("unsupported receipt journal version %d", snapshot.Version)
	}
	for id, record := range snapshot.Records {
		if id != record.Receipt.ReceiptID {
			return nil, errors.New("receipt journal key does not match receipt_id")
		}
		if err := record.Receipt.Verify(); err != nil {
			return nil, fmt.Errorf("verify persisted receipt %s: %w", id, err)
		}
		journal.records[id] = record
	}
	return journal, nil
}

func (j *FileJournal) Put(receipt Receipt) (bool, error) {
	if err := receipt.Verify(); err != nil {
		return false, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if existing, ok := j.records[receipt.ReceiptID]; ok {
		if !sameReceipt(existing.Receipt, receipt) {
			return false, errors.New("receipt id conflicts with existing journal record")
		}
		return false, nil
	}
	j.records[receipt.ReceiptID] = JournalRecord{Receipt: receipt}
	if err := j.persistLocked(); err != nil {
		delete(j.records, receipt.ReceiptID)
		return false, err
	}
	return true, nil
}

func (j *FileJournal) Acknowledge(receiptID string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.records[receiptID]
	if !ok {
		return ErrReceiptNotFound
	}
	if !record.AcknowledgedAt.IsZero() {
		return nil
	}
	record.AcknowledgedAt = at.UTC()
	j.records[receiptID] = record
	if err := j.persistLocked(); err != nil {
		record.AcknowledgedAt = time.Time{}
		j.records[receiptID] = record
		return err
	}
	return nil
}

func (j *FileJournal) Get(receiptID string) (JournalRecord, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	record, ok := j.records[receiptID]
	if !ok {
		return JournalRecord{}, ErrReceiptNotFound
	}
	return cloneJournalRecord(record), nil
}

func (j *FileJournal) Pending() []Receipt {
	j.mu.RLock()
	defer j.mu.RUnlock()
	var receipts []Receipt
	for _, record := range j.records {
		if record.AcknowledgedAt.IsZero() {
			receipts = append(receipts, record.Receipt)
		}
	}
	sortReceipts(receipts)
	return receipts
}

func (j *FileJournal) Records() []JournalRecord {
	j.mu.RLock()
	defer j.mu.RUnlock()
	records := make([]JournalRecord, 0, len(j.records))
	for _, record := range j.records {
		records = append(records, cloneJournalRecord(record))
	}
	sort.Slice(records, func(i, k int) bool { return records[i].Receipt.ReceiptID < records[k].Receipt.ReceiptID })
	return records
}

func (j *FileJournal) persistLocked() error {
	directory := filepath.Dir(j.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".receipts-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	compacted := compactAcknowledgedReceipts(j.records, maxRetainedAcknowledgedReceipts)
	if err := json.NewEncoder(temporary).Encode(fileSnapshot{Version: journalVersion, Records: compacted}); err != nil {
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
	if err := os.Rename(temporaryPath, j.path); err != nil {
		return err
	}
	j.records = compacted
	directoryHandle, err := os.Open(directory)
	if err == nil {
		err = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return err
}

func compactAcknowledgedReceipts(records map[string]JournalRecord, limit int) map[string]JournalRecord {
	if limit < 0 {
		limit = 0
	}
	acknowledged := make([]string, 0)
	for id, record := range records {
		if !record.AcknowledgedAt.IsZero() {
			acknowledged = append(acknowledged, id)
		}
	}
	if len(acknowledged) <= limit {
		return records
	}
	sort.Slice(acknowledged, func(i, k int) bool {
		left, right := records[acknowledged[i]], records[acknowledged[k]]
		if left.AcknowledgedAt.Equal(right.AcknowledgedAt) {
			return acknowledged[i] > acknowledged[k]
		}
		return left.AcknowledgedAt.After(right.AcknowledgedAt)
	})
	compacted := make(map[string]JournalRecord, len(records)-(len(acknowledged)-limit))
	for id, record := range records {
		compacted[id] = record
	}
	for _, id := range acknowledged[limit:] {
		delete(compacted, id)
	}
	return compacted
}

func sameReceipt(left, right Receipt) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func cloneJournalRecord(record JournalRecord) JournalRecord {
	encoded, err := json.Marshal(record)
	if err != nil {
		return record
	}
	var cloned JournalRecord
	if json.Unmarshal(encoded, &cloned) != nil {
		return record
	}
	return cloned
}

func sortReceipts(receipts []Receipt) {
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].IssuedAt.Equal(receipts[j].IssuedAt) {
			return receipts[i].ReceiptID < receipts[j].ReceiptID
		}
		return receipts[i].IssuedAt.Before(receipts[j].IssuedAt)
	})
}

var _ Journal = (*FileJournal)(nil)
