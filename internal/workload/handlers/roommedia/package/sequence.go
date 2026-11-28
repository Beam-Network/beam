package packager

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

var ErrPackagerStateLost = errors.New("packager_state_lost: the restarted packager lost its persisted media sequence")

const (
	msnReserveBlock      = 16
	sequenceReserveBlock = 1024
)

type msnStore struct {
	path string

	mu    sync.Mutex
	state msnState
}

type msnState struct {
	MSN      map[string]int64 `json:"msn"`
	Sequence map[string]int64 `json:"sequence"`
}

func openMSNStore(env *broadcast.Env) (*msnStore, error) {
	store := &msnStore{state: msnState{MSN: map[string]int64{}, Sequence: map[string]int64{}}}
	if env.StateDir == "" {
		return store, nil
	}
	store.path = filepath.Join(env.StateDir, "package-sequences.json")
	if env.Resumed || env.Redelivery {
		var persisted msnState
		data, err := os.ReadFile(store.path)
		if err == nil {
			err = json.Unmarshal(data, &persisted)
		}
		if err == nil && persisted.MSN != nil && persisted.Sequence != nil {
			store.state = persisted
			return store, nil
		}
		if !env.Redelivery {
			return nil, ErrPackagerStateLost
		}
		log.Printf("media package %s: fresh offer without persisted sequences: starting at its floor", env.Node.NodeID)
	}
	if err := store.write(); err != nil {
		return nil, fmt.Errorf("packager state: %w", err)
	}
	return store, nil
}

func (s *msnStore) start(rendition string, sessionStart int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reserved, ok := s.state.MSN[rendition]; ok {
		return max(sessionStart, reserved+1)
	}
	return sessionStart
}

func (s *msnStore) sequenceStart(rendition string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Sequence[rendition]
}

func (s *msnStore) useMSN(rendition string, msn int64) {
	s.reserve(s.state.MSN, rendition, msn, msnReserveBlock)
}

func (s *msnStore) useSequence(rendition string, sequence int64) {
	s.reserve(s.state.Sequence, rendition, sequence, sequenceReserveBlock)
}

func (s *msnStore) reserve(marks map[string]int64, rendition string, value, block int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if marks[rendition] >= value {
		return
	}
	marks[rendition] = value + block - 1
	if err := s.write(); err != nil && s.path != "" {
		log.Printf("media package: persist sequences: %v", err)
		_ = os.Remove(s.path)
	}
}

func (s *msnStore) write() error {
	if s.path == "" {
		return nil
	}
	encoded, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	return broadcast.WriteFileDurable(s.path, encoded)
}

func sessionStartMSN(env *broadcast.Env, started time.Time) int64 {
	if floor := env.Node.Spec.Details.MediaSequenceFloor; floor != nil {
		return *floor
	}
	return started.UnixMilli() / 100
}
