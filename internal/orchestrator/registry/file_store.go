package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
)

const stateVersion = 1

type State struct {
	FormatVersion int                                    `json:"format_version"`
	Version       uint64                                 `json:"version"`
	Orchestrator  orchestratordomain.Orchestrator        `json:"orchestrator"`
	Memberships   []orchestratordomain.Membership        `json:"memberships"`
	Observations  []orchestratordomain.WorkerObservation `json:"observations"`
}

type StateStore interface {
	Load() (State, error)
	Save(State) error
}

type FileStateStore struct{ Path string }

func (s FileStateStore) Load() (State, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode Orchestrator registry: %w", err)
	}
	if state.FormatVersion != stateVersion {
		return State{}, fmt.Errorf("unsupported Orchestrator registry version %d", state.FormatVersion)
	}
	return state, nil
}

func (s FileStateStore) Save(state State) error {
	if s.Path == "" {
		return errors.New("Orchestrator registry path is required")
	}
	state.FormatVersion = stateVersion
	directory := filepath.Dir(s.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".orchestrator-registry-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
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
	if err := os.Rename(temporaryPath, s.Path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err == nil {
		err = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return err
}
