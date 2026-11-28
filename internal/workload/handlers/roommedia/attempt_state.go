package roommedia

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	attemptStateDir = "media-attempts"
	eventIDBlock    = 64
)

type AttemptState struct {
	Dir        string
	Resumed    bool
	Redelivery bool
}

func (h *Handler) attemptState(identity contracts.RoomWorkloadIdentity, resumed, redelivery bool) *AttemptState {
	if strings.TrimSpace(h.config.StateDir) == "" {
		return nil
	}
	root := filepath.Join(h.config.StateDir, attemptStateDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil
	}
	pruneAttemptStates(root, h.now())
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", identity.WorkloadID, identity.UnitID,
		identity.Epoch, identity.Attempt)))
	dir := filepath.Join(root, hex.EncodeToString(digest[:16]))
	_, statErr := os.Stat(dir)
	existed := statErr == nil
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	return &AttemptState{Dir: dir, Resumed: existed || resumed, Redelivery: redelivery}
}

func (h *Handler) pruneStates() {
	if strings.TrimSpace(h.config.StateDir) == "" {
		return
	}
	now := h.now()
	pruneAttemptStates(filepath.Join(h.config.StateDir, attemptStateDir), now)
	pruneUsageSequences(filepath.Join(h.config.StateDir, usageSequenceDir), now)
}

const statePruneInterval = time.Hour

func (h *Handler) runStatePrune(stop <-chan struct{}) {
	h.pruneStates()
	ticker := time.NewTicker(statePruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			h.pruneStates()
		}
	}
}

type eventIDs struct {
	prefix string
	path   string
	random bool

	mu       sync.Mutex
	next     uint64
	reserved uint64
}

const eventIDRandomBytes = 8

func newEventIDs(identity contracts.RoomWorkloadIdentity, state *AttemptState) *eventIDs {
	ids := &eventIDs{prefix: mediacontracts.SessionEventID(int64(identity.Epoch), int64(identity.Attempt), "")}
	resumed := false
	if state != nil {
		ids.path = filepath.Join(state.Dir, "events")
		if data, err := os.ReadFile(ids.path); err == nil {
			if value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
				ids.next, ids.reserved, resumed = value, value, true
			}
		}
	}
	if !resumed {
		ids.addRandomPart()
	}
	return ids
}

func (ids *eventIDs) addRandomPart() {
	random := make([]byte, eventIDRandomBytes)
	_, _ = rand.Read(random)
	ids.prefix += hex.EncodeToString(random) + "-"
	ids.random = true
}

func (ids *eventIDs) Next() string {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if ids.path != "" && ids.next >= ids.reserved {
		reserved := ids.next + eventIDBlock
		if err := writeFileAtomic(ids.path, []byte(strconv.FormatUint(reserved, 10))); err != nil {
			if !ids.random {
				ids.addRandomPart()
			}
			ids.path = ""
		} else {
			ids.reserved = reserved
		}
	}
	ids.next++
	return fmt.Sprintf("%s%08d", ids.prefix, ids.next)
}

func pruneAttemptStates(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && now.Sub(info.ModTime()) > usageSequenceRetention {
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}
}

func (s *AttemptState) Remove() {
	if s != nil && s.Dir != "" {
		_ = os.RemoveAll(s.Dir)
	}
}

func nodeKey(state *AttemptState) (ed25519.PrivateKey, error) {
	if state == nil {
		return usage.NewKey()
	}
	path := filepath.Join(state.Dir, "node-key")
	if data, err := os.ReadFile(path); err == nil {
		if seed, err := hex.DecodeString(strings.TrimSpace(string(data))); err == nil && len(seed) == ed25519.SeedSize {
			return ed25519.NewKeyFromSeed(seed), nil
		}
	}
	key, err := usage.NewKey()
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, []byte(hex.EncodeToString(key.Seed()))); err != nil {
		return nil, fmt.Errorf("keep the node key: %w", err)
	}
	return key, nil
}

func writeFileAtomic(path string, data []byte) error { return broadcast.WriteFileDurable(path, data) }
