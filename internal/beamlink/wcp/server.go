package wcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/evidence"
	orchestratorcontrol "github.com/Beam-Network/beam/internal/orchestrator/control"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type ResultEvent struct {
	WorkerID string
	Result   domain.Result
}

type ProgressEvent struct {
	WorkerID string
	Progress domain.Progress
}

type CheckpointEvent struct {
	WorkerID   string
	Checkpoint domain.Checkpoint
}

type ReceiptEvent struct {
	WorkerID string
	Receipt  evidence.Receipt
}

type Server struct {
	orchestratorID string
	registry       *registry.Registry
	control        *orchestratorcontrol.Service

	mu          sync.RWMutex
	sessions    map[string]*Session
	pending     map[string]chan runtime.Decision
	results     chan ResultEvent
	progress    chan ProgressEvent
	checkpoints chan CheckpointEvent
	receipts    chan ReceiptEvent
	journal     Journal
	circuits    map[string]circuit.Plan
	revoked     map[string]revokedCircuit
}

type revokedCircuit struct {
	Plan       circuit.Plan
	Revocation circuit.Revocation
}

func NewServer(orchestratorID string, orchestratorRegistry *registry.Registry, configEpoch uint64) (*Server, error) {
	return NewServerWithJournal(orchestratorID, orchestratorRegistry, configEpoch, newMemoryJournal())
}

func NewServerWithJournal(orchestratorID string, orchestratorRegistry *registry.Registry, configEpoch uint64, journal Journal) (*Server, error) {
	controlService, err := orchestratorcontrol.NewService(orchestratorID, orchestratorRegistry, configEpoch)
	if err != nil {
		return nil, err
	}
	if journal == nil {
		journal = newMemoryJournal()
	}
	server := &Server{
		orchestratorID: orchestratorID, registry: orchestratorRegistry, control: controlService,
		sessions: make(map[string]*Session), pending: make(map[string]chan runtime.Decision),
		results: make(chan ResultEvent, 256), progress: make(chan ProgressEvent, 256), checkpoints: make(chan CheckpointEvent, 256), receipts: make(chan ReceiptEvent, 256), journal: journal,
		circuits: make(map[string]circuit.Plan), revoked: make(map[string]revokedCircuit),
	}
	if err := server.restoreCircuits(journal.Events()); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *Server) Results() <-chan ResultEvent         { return s.results }
func (s *Server) Progress() <-chan ProgressEvent      { return s.progress }
func (s *Server) Checkpoints() <-chan CheckpointEvent { return s.checkpoints }
func (s *Server) Receipts() <-chan ReceiptEvent       { return s.receipts }
func (s *Server) Journal() Journal                    { return s.journal }

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(ctx, connection)
	}
}

func (s *Server) Offer(ctx context.Context, workerID string, spec domain.Spec) (runtime.Decision, error) {
	session, err := s.session(workerID)
	if err != nil {
		return runtime.Decision{}, err
	}
	response := make(chan runtime.Decision, 1)
	if _, err := s.journal.Append(JournalEvent{
		EventID: randomID("offer"), WorkerID: workerID, Type: TypeOffer,
		WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID,
	}); err != nil {
		return runtime.Decision{}, err
	}
	s.mu.Lock()
	if s.sessions[workerID] != session {
		s.mu.Unlock()
		return runtime.Decision{}, errors.New("Worker WCP session changed")
	}
	request, err := session.send(TypeOffer, "", spec)
	if err != nil {
		s.mu.Unlock()
		return runtime.Decision{}, err
	}
	s.pending[request.MessageID] = response
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, request.MessageID)
		s.mu.Unlock()
	}()
	select {
	case decision := <-response:
		return decision, nil
	case <-ctx.Done():
		return runtime.Decision{}, ctx.Err()
	case <-session.done:
		return runtime.Decision{}, errors.New("Worker WCP session closed")
	}
}

func (s *Server) Commit(_ context.Context, workerID string, commit domain.Commit) error {
	session, err := s.session(workerID)
	if err != nil {
		return err
	}
	if _, err := s.journal.Append(JournalEvent{
		EventID: randomID("commit"), WorkerID: workerID, Type: TypeCommit,
		WorkloadID: commit.WorkloadID, AttemptID: commit.AttemptID,
	}); err != nil {
		return err
	}
	_, err = session.send(TypeCommit, "", commit)
	return err
}

func (s *Server) Cancel(_ context.Context, workerID, workloadID, attemptID string) error {
	session, err := s.session(workerID)
	if err != nil {
		return err
	}
	if _, err := s.journal.Append(JournalEvent{
		EventID: randomID("cancel"), WorkerID: workerID, Type: TypeCancel,
		WorkloadID: workloadID, AttemptID: attemptID,
	}); err != nil {
		return err
	}
	_, err = session.send(TypeCancel, "", Cancel{WorkloadID: workloadID, AttemptID: attemptID})
	return err
}

func (s *Server) Connected(workerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.sessions[workerID]
	return ok
}

func (s *Server) session(workerID string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[workerID]
	if !ok {
		return nil, fmt.Errorf("worker %s has no active WCP session", workerID)
	}
	return session, nil
}
