package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/orchestrator/routing"
	"github.com/Beam-Network/beam/internal/orchestrator/scheduling"
	workload "github.com/Beam-Network/beam/internal/workload/domain"
	workloadruntime "github.com/Beam-Network/beam/internal/workload/runtime"
)

const maxBodyBytes = 1 << 20

type Server struct {
	orchestratorID string
	registry       *registry.Registry
	scheduler      *scheduling.Scheduler
	now            func() time.Time
	control        ControlPlane
	router         *routing.Orchestrator
	tasks          *dispatch.Service
}

type ControlPlane interface {
	Offer(context.Context, string, workload.Spec) (workloadruntime.Decision, error)
	Commit(context.Context, string, workload.Commit) error
	Cancel(context.Context, string, string, string) error
	Connected(string) bool
	UpsertCircuit(context.Context, circuit.Plan) error
	RevokeCircuit(context.Context, circuit.Revocation) error
}

func New(orchestratorID string, orchestratorRegistry *registry.Registry) *Server {
	return &Server{orchestratorID: orchestratorID, registry: orchestratorRegistry, scheduler: scheduling.New(orchestratorRegistry), now: time.Now}
}

func NewWithControl(orchestratorID string, orchestratorRegistry *registry.Registry, control ControlPlane) *Server {
	server := New(orchestratorID, orchestratorRegistry)
	server.control = control
	server.router = routing.NewOrchestrator(orchestratorRegistry, control)
	return server
}

// AttachOrchestration exposes read-only owner-local task state and an event
// stream. Task ingress intentionally remains on the three NATS connectors.
func (s *Server) AttachOrchestration(tasks *dispatch.Service) { s.tasks = tasks }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/orchestrator/manifest", s.manifest)
	mux.HandleFunc("POST /v1/orchestrator/memberships", s.join)
	mux.HandleFunc("POST /v1/orchestrator/observations", s.observe)
	mux.HandleFunc("POST /v1/orchestrator/placements", s.place)
	mux.HandleFunc("POST /v1/orchestrator/workloads/offers", s.offer)
	mux.HandleFunc("POST /v1/orchestrator/workloads/commits", s.commit)
	mux.HandleFunc("POST /v1/orchestrator/workloads/cancel", s.cancel)
	mux.HandleFunc("POST /v1/orchestrator/circuits", s.upsertCircuit)
	mux.HandleFunc("POST /v1/orchestrator/circuits/revoke", s.revokeCircuit)
	mux.HandleFunc("POST /v1/orchestrator/transfers/plan", s.planDistribution)
	mux.HandleFunc("POST /v1/orchestrator/transfers/dispatch", s.dispatchDistribution)
	mux.HandleFunc("POST /v1/orchestrator/network/links", s.observeLinks)
	mux.HandleFunc("GET /v1/orchestrator/tasks", s.listTasks)
	mux.HandleFunc("GET /v1/orchestrator/task-events", s.taskEvents)
	return mux
}

func (s *Server) listTasks(response http.ResponseWriter, _ *http.Request) {
	if s.tasks == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "NATS task orchestration is disabled"})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"tasks": s.tasks.Records()})
}

func (s *Server) taskEvents(response http.ResponseWriter, request *http.Request) {
	if s.tasks == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "NATS task orchestration is disabled"})
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeJSON(response, http.StatusInternalServerError, map[string]string{"error": "streaming is unavailable"})
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Accel-Buffering", "no")
	events, unsubscribe := s.tasks.Subscribe(64)
	defer unsubscribe()
	_, _ = io.WriteString(response, ": beam-orchestrator task events\n\n")
	flusher.Flush()
	for {
		select {
		case <-request.Context().Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(response, "id: %d\nevent: task\ndata: %s\n\n", event.Sequence, encoded)
			flusher.Flush()
		}
	}
}

func (s *Server) observeLinks(response http.ResponseWriter, request *http.Request) {
	if s.router == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP routing is disabled"})
		return
	}
	var input struct {
		Links []routing.LinkMetric `json:"links"`
	}
	if !decode(response, request, &input) {
		return
	}
	if err := s.router.ObserveLinks(input.Links, s.now()); err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{"status": "observed", "links": len(input.Links)})
}

func (s *Server) planDistribution(response http.ResponseWriter, request *http.Request) {
	if s.router == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP routing is disabled"})
		return
	}
	var input routing.Request
	if !decode(response, request, &input) {
		return
	}
	plan, err := s.router.Plan(input, s.now())
	if err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, plan)
}

func (s *Server) dispatchDistribution(response http.ResponseWriter, request *http.Request) {
	if s.router == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP routing is disabled"})
		return
	}
	var input routing.Request
	if !decode(response, request, &input) {
		return
	}
	plan, err := s.router.Dispatch(request.Context(), input, s.now())
	if err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, plan)
}

func (s *Server) upsertCircuit(response http.ResponseWriter, request *http.Request) {
	if s.control == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP control is disabled"})
		return
	}
	var plan circuit.Plan
	if !decode(response, request, &plan) {
		return
	}
	if err := s.control.UpsertCircuit(request.Context(), plan); err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{
		"status": "distributed", "circuit_id": plan.CircuitID, "plan_version": plan.PlanVersion,
	})
}

func (s *Server) revokeCircuit(response http.ResponseWriter, request *http.Request) {
	if s.control == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP control is disabled"})
		return
	}
	var revocation circuit.Revocation
	if !decode(response, request, &revocation) {
		return
	}
	if err := s.control.RevokeCircuit(request.Context(), revocation); err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{
		"status": "revoked", "circuit_id": revocation.CircuitID, "plan_version": revocation.PlanVersion,
	})
}

func (s *Server) offer(response http.ResponseWriter, request *http.Request) {
	if s.control == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP control is disabled"})
		return
	}
	var input struct {
		WorkerID string        `json:"worker_id"`
		Spec     workload.Spec `json:"spec"`
	}
	if !decode(response, request, &input) {
		return
	}
	if input.WorkerID == "" || input.Spec.Identity.WorkerID != input.WorkerID {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "worker_id must match workload identity"})
		return
	}
	decision, err := s.control.Offer(request.Context(), input.WorkerID, input.Spec)
	if err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, decision)
}

func (s *Server) commit(response http.ResponseWriter, request *http.Request) {
	if s.control == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP control is disabled"})
		return
	}
	var input struct {
		WorkerID string          `json:"worker_id"`
		Commit   workload.Commit `json:"commit"`
	}
	if !decode(response, request, &input) {
		return
	}
	if err := s.control.Commit(request.Context(), input.WorkerID, input.Commit); err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "committed"})
}

func (s *Server) cancel(response http.ResponseWriter, request *http.Request) {
	if s.control == nil {
		writeJSON(response, http.StatusNotImplemented, map[string]string{"error": "WCP control is disabled"})
		return
	}
	var input struct {
		WorkerID   string `json:"worker_id"`
		WorkloadID string `json:"workload_id"`
		AttemptID  string `json:"attempt_id"`
	}
	if !decode(response, request, &input) {
		return
	}
	if err := s.control.Cancel(request.Context(), input.WorkerID, input.WorkloadID, input.AttemptID); err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "cancellation_requested"})
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok", "orchestrator_id": s.orchestratorID})
}

func (s *Server) manifest(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.registry.Manifest(s.now(), time.Minute, nil))
}

func (s *Server) join(response http.ResponseWriter, request *http.Request) {
	var membership orchestratordomain.Membership
	if !decode(response, request, &membership) {
		return
	}
	if err := s.registry.Join(membership, s.now()); err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusCreated, membership)
}

func (s *Server) observe(response http.ResponseWriter, request *http.Request) {
	var observation orchestratordomain.WorkerObservation
	if !decode(response, request, &observation) {
		return
	}
	if err := s.registry.UpdateObservation(observation, s.now()); err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusAccepted, observation)
}

func (s *Server) place(response http.ResponseWriter, request *http.Request) {
	var input struct {
		RequiredCapabilities []string           `json:"required_capabilities"`
		Resources            workload.Resources `json:"resources"`
	}
	if !decode(response, request, &input) {
		return
	}
	placement, err := s.scheduler.Select(scheduling.Request{
		RequiredCapabilities: input.RequiredCapabilities, Resources: input.Resources,
	}, s.now())
	if err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]any{"error": err.Error(), "placement": placement})
		return
	}
	writeJSON(response, http.StatusOK, placement)
}

func decode(response http.ResponseWriter, request *http.Request, destination any) bool {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
