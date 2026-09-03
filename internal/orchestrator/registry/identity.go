package registry

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
)

// ResolveOrchestratorID treats an explicit value as a migration/recovery override,
// otherwise it restores the persisted identity or generates a new one. The
// generated value becomes durable when NewWithStore initializes the registry.
func ResolveOrchestratorID(configured string, store StateStore) (orchestratorID string, generated bool, err error) {
	if value := strings.TrimSpace(configured); value != "" {
		return value, false, nil
	}
	if store == nil {
		return "", false, errors.New("Orchestrator registry store is required to resolve an automatic orchestrator_id")
	}
	state, err := store.Load()
	if err != nil {
		return "", false, err
	}
	if value := strings.TrimSpace(state.Orchestrator.OrchestratorID); value != "" {
		return value, false, nil
	}
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", false, err
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(value)
	return "orchestrator_" + strings.ToLower(encoded), true, nil
}
