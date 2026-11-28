package roommedia

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	usageSequenceDir       = "media-usage"
	usageSequenceRetention = 25 * time.Hour
)

type fileSequenceStore struct {
	path string
}

func (h *Handler) sequenceStore(identity contracts.RoomWorkloadIdentity) usage.SequenceStore {
	if strings.TrimSpace(h.config.StateDir) == "" {
		return nil
	}
	directory := filepath.Join(h.config.StateDir, usageSequenceDir)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil
	}
	pruneUsageSequences(directory, h.now())
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", identity.WorkloadID, identity.UnitID,
		identity.Epoch, identity.Attempt)))
	return &fileSequenceStore{path: filepath.Join(directory, hex.EncodeToString(digest[:16]))}
}

func (s *fileSequenceStore) Load() int64 {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return 0
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func (s *fileSequenceStore) Save(sequence int64) error {
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, []byte(strconv.FormatInt(sequence, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

const periodSuffix = ".period"

func (s *fileSequenceStore) LoadPeriodEnd() int64 {
	return (&fileSequenceStore{path: s.path + periodSuffix}).Load()
}

func (s *fileSequenceStore) SavePeriodEnd(endMS int64) error {
	return (&fileSequenceStore{path: s.path + periodSuffix}).Save(endMS)
}

func pruneUsageSequences(directory string, now time.Time) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && now.Sub(info.ModTime()) > usageSequenceRetention {
			_ = os.Remove(filepath.Join(directory, entry.Name()))
		}
	}
}
