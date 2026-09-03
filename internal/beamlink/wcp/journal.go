package wcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/evidence"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type JournalEvent struct {
	EventID           string              `json:"event_id"`
	WorkerID          string              `json:"worker_id"`
	Type              string              `json:"type"`
	RecordedAt        time.Time           `json:"recorded_at"`
	WorkloadID        string              `json:"workload_id,omitempty"`
	AttemptID         string              `json:"attempt_id,omitempty"`
	Result            *domain.Result      `json:"result,omitempty"`
	Progress          *domain.Progress    `json:"progress,omitempty"`
	Checkpoint        *domain.Checkpoint  `json:"checkpoint,omitempty"`
	CircuitPlan       *circuit.Plan       `json:"circuit_plan,omitempty"`
	CircuitRevocation *circuit.Revocation `json:"circuit_revocation,omitempty"`
	Receipt           *evidence.Receipt   `json:"receipt,omitempty"`
}

type Journal interface {
	Append(JournalEvent) (bool, error)
	Events() []JournalEvent
}

type FileJournal struct {
	mu     sync.Mutex
	path   string
	events []JournalEvent
	seen   map[string]struct{}
}

func OpenFileJournal(path string) (*FileJournal, error) {
	if path == "" {
		return nil, errors.New("WCP journal path is required")
	}
	journal := &FileJournal{path: path, seen: make(map[string]struct{})}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return journal, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64<<10)
	scanner.Buffer(buffer, maxFrameBytes*2)
	for scanner.Scan() {
		var event JournalEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		if event.EventID == "" {
			return nil, errors.New("WCP journal contains an event without id")
		}
		if _, exists := journal.seen[event.EventID]; exists {
			continue
		}
		journal.seen[event.EventID] = struct{}{}
		journal.events = append(journal.events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return journal, nil
}

func (j *FileJournal) Append(event JournalEvent) (bool, error) {
	if event.EventID == "" {
		return false, errors.New("WCP journal event id is required")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, exists := j.seen[event.EventID]; exists {
		return false, nil
	}
	if event.RecordedAt.IsZero() {
		event.RecordedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return false, err
	}
	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	encoded, err := json.Marshal(event)
	if err == nil {
		_, err = file.Write(append(encoded, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	j.seen[event.EventID] = struct{}{}
	j.events = append(j.events, event)
	return true, nil
}

func (j *FileJournal) Events() []JournalEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]JournalEvent, len(j.events))
	copy(result, j.events)
	return result
}

type memoryJournal struct {
	mu     sync.Mutex
	events []JournalEvent
	seen   map[string]struct{}
}

func newMemoryJournal() *memoryJournal { return &memoryJournal{seen: make(map[string]struct{})} }

func (j *memoryJournal) Append(event JournalEvent) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, exists := j.seen[event.EventID]; exists {
		return false, nil
	}
	j.seen[event.EventID] = struct{}{}
	j.events = append(j.events, event)
	return true, nil
}

func (j *memoryJournal) Events() []JournalEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]JournalEvent(nil), j.events...)
}
