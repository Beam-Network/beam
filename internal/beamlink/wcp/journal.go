package wcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
)

const maxJournalLineBytes = maxFrameBytes * 2

type JournalEvent struct {
	EventID           string              `json:"event_id"`
	Type              string              `json:"type"`
	RecordedAt        time.Time           `json:"recorded_at"`
	WorkloadID        string              `json:"workload_id,omitempty"`
	CircuitPlan       *circuit.Plan       `json:"circuit_plan,omitempty"`
	CircuitRevocation *circuit.Revocation `json:"circuit_revocation,omitempty"`
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
	retained, compact, err := journal.load(file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if compact {
		if err := replaceJournal(path, retained); err != nil {
			return nil, err
		}
	}
	return journal, nil
}

func (j *FileJournal) load(file *os.File) ([]byte, bool, error) {
	reader := bufio.NewReader(file)
	var retained bytes.Buffer
	compact := false
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return retained.Bytes(), compact || len(line) > 0, nil
		}
		if err != nil {
			return nil, false, err
		}
		var event JournalEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, false, err
		}
		if event.EventID == "" {
			return nil, false, errors.New("WCP journal contains an event without id")
		}
		if event.Type != TypeCircuitUpsert && event.Type != TypeCircuitRevoke {
			compact = true
			continue
		}
		if _, exists := j.seen[event.EventID]; exists {
			continue
		}
		j.seen[event.EventID] = struct{}{}
		j.events = append(j.events, event)
		retained.Write(line)
	}
}

func replaceJournal(path string, data []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".wcp-journal-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	_, err = temporary.Write(data)
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err == nil {
		err = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	return err
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
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(event); err != nil {
		return false, err
	}
	if encoded.Len() > maxJournalLineBytes {
		return false, errors.New("WCP journal event exceeds the journal line limit")
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return false, err
	}
	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	_, err = file.Write(encoded.Bytes())
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
