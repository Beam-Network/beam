package server

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/resources"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

const maxRequestBytes = 1 << 20

type Server struct {
	identity     domain.Identity
	capabilities []string
	engine       *runtime.Engine
	governor     *resources.Governor
	store        runtime.Store
	token        string
	circuits     *circuit.Service
}

func (s *Server) AttachCircuits(service *circuit.Service) { s.circuits = service }

func New(identity domain.Identity, capabilities []string, engine *runtime.Engine, governor *resources.Governor, store runtime.Store, token string) (*Server, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if engine == nil || governor == nil || store == nil {
		return nil, errors.New("engine, governor, and store are required")
	}
	return &Server{identity: identity, capabilities: capabilities, engine: engine, governor: governor, store: store, token: token}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("POST /v1/workloads/offers", s.offer)
	mux.HandleFunc("POST /v1/workloads/commits", s.commit)
	mux.HandleFunc("POST /v1/workloads/cancel", s.cancel)
	mux.HandleFunc("GET /v1/workloads", s.workloads)
	mux.HandleFunc("GET /v1/circuits", s.circuitPlans)
	mux.HandleFunc("POST /v1/circuits/dial", s.dialCircuit)
	mux.HandleFunc("POST /v1/circuits/accept", s.acceptCircuit)
	return s.authorize(mux)
}

type circuitSummary struct {
	CircuitID   string         `json:"circuit_id"`
	WorkloadID  string         `json:"workload_id"`
	PlanVersion uint64         `json:"plan_version"`
	Peers       []circuit.Peer `json:"peers"`
	ExpiresAt   string         `json:"expires_at"`
}

func (s *Server) circuitPlans(response http.ResponseWriter, _ *http.Request) {
	if s.circuits == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "circuits are disabled"})
		return
	}
	plans := s.circuits.Plans()
	sort.Slice(plans, func(i, j int) bool { return plans[i].CircuitID < plans[j].CircuitID })
	summaries := make([]circuitSummary, 0, len(plans))
	for _, plan := range plans {
		summaries = append(summaries, circuitSummary{
			CircuitID: plan.CircuitID, WorkloadID: plan.WorkloadID, PlanVersion: plan.PlanVersion,
			Peers: plan.Peers, ExpiresAt: plan.ExpiresAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(response, http.StatusOK, summaries)
}

func (s *Server) dialCircuit(response http.ResponseWriter, request *http.Request) {
	if s.circuits == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "circuits are disabled"})
		return
	}
	metadata, err := decodeCircuitMetadata(request.URL.Query().Get("metadata"))
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	connection, err := s.circuits.DialWithFailover(request.Context(), request.URL.Query().Get("circuit_id"),
		request.URL.Query().Get("peer_node_id"), request.URL.Query()["standby_node_id"], circuit.OpenRequest{
			ChannelID: request.URL.Query().Get("channel_id"), Protocol: request.URL.Query().Get("protocol"), Metadata: metadata,
		})
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	s.upgradeCircuit(response, connection)
}

func (s *Server) acceptCircuit(response http.ResponseWriter, request *http.Request) {
	if s.circuits == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "circuits are disabled"})
		return
	}
	protocol := request.URL.Query().Get("protocol")
	workloadID := request.URL.Query().Get("workload_id")
	connection, err := s.circuits.AcceptFor(request.Context(), protocol, workloadID)
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, err)
		return
	}
	s.upgradeCircuit(response, connection)
}

func (s *Server) upgradeCircuit(response http.ResponseWriter, connection *circuit.Conn) {
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		_ = connection.Close()
		writeJSON(response, http.StatusInternalServerError, map[string]string{"error": "HTTP transport does not support connection upgrade"})
		return
	}
	ownerConnection, buffer, err := hijacker.Hijack()
	if err != nil {
		_ = connection.Close()
		return
	}
	descriptor, _ := json.Marshal(map[string]any{
		"circuit_id": connection.CircuitID, "workload_id": connection.WorkloadID,
		"peer": connection.Peer, "channel_id": connection.ChannelID,
		"protocol": connection.Protocol, "metadata": connection.Metadata,
	})
	_, err = fmt.Fprintf(buffer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: beam-circuit\r\nX-Beam-Circuit: %s\r\n\r\n",
		base64.RawURLEncoding.EncodeToString(descriptor))
	if err == nil {
		err = buffer.Flush()
	}
	if err != nil {
		_ = ownerConnection.Close()
		_ = connection.Close()
		return
	}
	go bridgeConnections(&bufferedConnection{Conn: ownerConnection, reader: buffer.Reader}, connection)
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(destination []byte) (int, error) {
	return c.reader.Read(destination)
}

func decodeCircuitMetadata(encoded string) (map[string]string, error) {
	if encoded == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(data) > 64<<10 {
		return nil, errors.New("metadata must be bounded base64url JSON")
	}
	var metadata map[string]string
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, errors.New("metadata must be a JSON string map")
	}
	return metadata, nil
}

func bridgeConnections(left, right net.Conn) {
	closeBoth := func() {
		_ = left.Close()
		_ = right.Close()
	}
	go func() {
		_, _ = io.Copy(left, right)
		closeBoth()
	}()
	_, _ = io.Copy(right, left)
	closeBoth()
}

func (s *Server) authorize(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			next.ServeHTTP(response, request)
			return
		}
		got := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(s.token) || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok", "worker_id": s.identity.WorkerID})
}

func (s *Server) status(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]any{
		"identity": s.identity, "capabilities": s.capabilities,
		"resources": s.governor.Snapshot(), "workloads": len(s.store.List()),
	})
}

func (s *Server) offer(response http.ResponseWriter, request *http.Request) {
	var spec domain.Spec
	if err := decodeJSON(request, &spec); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	decision, err := s.engine.Offer(request.Context(), spec)
	if err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, decision)
		return
	}
	writeJSON(response, http.StatusAccepted, decision)
}

func (s *Server) commit(response http.ResponseWriter, request *http.Request) {
	var commit domain.Commit
	if err := decodeJSON(request, &commit); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := s.engine.Commit(request.Context(), commit); err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "committed", "workload_key": commit.Key()})
}

func (s *Server) cancel(response http.ResponseWriter, request *http.Request) {
	var input struct {
		WorkloadID string `json:"workload_id"`
		AttemptID  string `json:"attempt_id"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	key := input.WorkloadID + "/" + input.AttemptID
	if err := s.engine.Cancel(key); err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "cancellation_requested", "workload_key": key})
}

type workloadSummary struct {
	WorkloadID  string             `json:"workload_id"`
	AttemptID   string             `json:"attempt_id"`
	Kind        domain.Kind        `json:"kind"`
	State       domain.State       `json:"state"`
	Reason      string             `json:"reason,omitempty"`
	PlanVersion uint64             `json:"plan_version,omitempty"`
	Progress    *domain.Progress   `json:"progress,omitempty"`
	Checkpoint  *domain.Checkpoint `json:"checkpoint,omitempty"`
}

func (s *Server) workloads(response http.ResponseWriter, _ *http.Request) {
	records := s.store.List()
	summaries := make([]workloadSummary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, workloadSummary{
			WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID,
			Kind: record.Spec.Kind, State: record.State, Reason: record.Reason,
			PlanVersion: record.PlanVersion, Progress: record.Progress,
			Checkpoint: record.Checkpoint,
		})
	}
	writeJSON(response, http.StatusOK, summaries)
}

func decodeJSON(request *http.Request, destination any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	return nil
}

func writeError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]string{"error": err.Error()})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
