package connectors

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	studioadapter "github.com/Beam-Network/beam/internal/workload/adapters/studio"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type StudioMessage struct {
	EventID string             `json:"event_id"`
	Type    string             `json:"type"`
	Task    studioadapter.Task `json:"task"`
}

type StudioResult struct {
	Type        string            `json:"type"`
	EventID     string            `json:"event_id"`
	TaskID      string            `json:"task_id"`
	AttemptID   string            `json:"attempt_id"`
	WorkerID    string            `json:"worker_id"`
	Status      string            `json:"status"`
	Outputs     map[string]string `json:"outputs,omitempty"`
	Error       string            `json:"error,omitempty"`
	CompletedAt string            `json:"completed_at"`
}

type StudioConnector struct {
	config       NATSConfig
	orchestrator *dispatch.Service
	nats         *natsConnector
}

func NewStudioConnector(config NATSConfig, orchestrator *dispatch.Service) (*StudioConnector, error) {
	if orchestrator == nil {
		return nil, errors.New("Orchestrator orchestration service is required")
	}
	if config.Name == "" {
		config.Name = "beam-orchestrator-studio"
	}
	if config.ResultSubject == "" {
		return nil, errors.New("Studio result NATS subject is required")
	}
	return &StudioConnector{config: config, orchestrator: orchestrator}, nil
}

func (s *StudioConnector) Run(ctx context.Context) error {
	session, err := connectNATS(s.config)
	if err != nil {
		return err
	}
	s.nats = session
	s.orchestrator.RegisterSink(dispatch.SourceStudio, s)
	defer func() {
		s.orchestrator.RegisterSink(dispatch.SourceStudio, nil)
		session.close()
	}()
	s.orchestrator.ReplayResults(ctx)
	return session.consume(ctx, s.handle)
}

func (s *StudioConnector) handle(ctx context.Context, encoded []byte) error {
	var message StudioMessage
	if err := json.Unmarshal(encoded, &message); err != nil {
		return err
	}
	if message.Type != "workflow_task" || message.Task.TaskID == "" {
		return errors.New("Studio NATS message must contain a locked workflow_task snapshot")
	}
	externalID := message.EventID
	if externalID == "" {
		externalID = message.Task.AttemptID
	}
	spec, err := studioadapter.ToWorkload(message.Task, domain.Identity{}, now())
	if err != nil {
		return err
	}
	_, err = s.orchestrator.Dispatch(ctx, dispatch.DispatchRequest{Source: dispatch.SourceStudio,
		ExternalID: externalID, Spec: spec})
	return err
}

func (s *StudioConnector) DeliverResult(ctx context.Context, record dispatch.Record, result domain.Result) error {
	status := "failed"
	if result.State == domain.StateCompleted {
		status = "completed"
	} else if result.State == domain.StateCancelled || result.State == domain.StateExpired {
		status = "cancelled"
	}
	payload, err := json.Marshal(StudioResult{Type: "workflow_task_result", EventID: record.ExternalID,
		TaskID: result.WorkloadID, AttemptID: result.AttemptID, WorkerID: record.WorkerID, Status: status,
		Outputs: result.Outputs, Error: result.ErrorMessage, CompletedAt: result.CompletedAt.UTC().Format(timeLayout)})
	if err != nil {
		return err
	}
	return acknowledged(ctx, s.nats, s.config.ResultSubject, payload, "Studio")
}
