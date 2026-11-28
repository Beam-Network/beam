package roomworkloads

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

func (m *Manager) MediaHandler(hotkey func() string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orchestrator/media/capacity", func(response http.ResponseWriter, _ *http.Request) {
		report, _ := m.MediaCapacityReport(hotkey(), time.Now().UTC())
		encoded, err := mediacontracts.MarshalWire(report)
		if err != nil {
			writeMediaJSON(response, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(encoded)
	})
	mux.HandleFunc("GET /v1/orchestrator/media/nodes", func(response http.ResponseWriter, _ *http.Request) {
		nodes := m.MediaNodes()
		if nodes == nil {
			nodes = []MediaNode{}
		}
		writeMediaJSON(response, http.StatusOK, map[string]any{"nodes": nodes, "draining_workers": m.MediaDrainingWorkers()})
	})
	mux.HandleFunc("POST /v1/orchestrator/media/drain", func(response http.ResponseWriter, request *http.Request) {
		var input struct {
			WorkerID string `json:"worker_id"`
			Drain    *bool  `json:"drain"`
		}
		decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<16))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || input.Drain == nil {
			writeMediaJSON(response, http.StatusBadRequest, map[string]string{"error": `want {"worker_id": "...", "drain": true|false}`})
			return
		}
		if err := m.DrainMediaWorker(input.WorkerID, *input.Drain); err != nil {
			writeMediaJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeMediaJSON(response, http.StatusAccepted, map[string]any{"worker_id": input.WorkerID, "draining": *input.Drain})
	})
	return mux
}

func writeMediaJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
