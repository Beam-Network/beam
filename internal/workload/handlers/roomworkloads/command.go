package roomworkloads

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/fanout"
)

type commandInvocation struct {
	CommandID string         `json:"command_id"`
	Command   string         `json:"command"`
	Request   map[string]any `json:"request"`
	ExpiresAt time.Time      `json:"expires_at"`
}
type commandBranch struct {
	target    contracts.RoomWorkerTarget
	commandID string
}
type commandDriver struct {
	handler    *Handler
	invocation commandInvocation
	timeout    time.Duration
}

func (d commandDriver) BranchID(value commandBranch) string { return value.target.MemberID }
func (d commandDriver) GroupKey(commandBranch) string       { return "command" }
func (d commandDriver) Read(context.Context, contracts.RoomPathLease, commandBranch) ([]byte, error) {
	return json.Marshal(d.invocation)
}
func (d commandDriver) Deliver(ctx context.Context, value commandBranch, payload []byte) (contracts.CommandOutcome, error) {
	requestContext, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	var response map[string]any
	err := d.handler.send(requestContext, value.target.Path, payload, 64<<10, &response)
	if err != nil {
		return contracts.CommandOutcome{TargetMemberID: value.target.MemberID, State: "failed", Reason: err.Error()}, nil
	}
	state := "acknowledged"
	if len(response) > 0 {
		state = "responded"
	}
	return contracts.CommandOutcome{TargetMemberID: value.target.MemberID, State: state, Response: response}, nil
}

func (h *Handler) executeCommand(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	assignment, _ := decodeSpec[contracts.CommandUnitDetails](spec)
	timeout := time.Duration(assignment.Details.TimeoutMS) * time.Millisecond
	invocation := commandInvocation{CommandID: assignment.Details.CommandID, Command: assignment.Details.Command,
		Request: assignment.Details.Request, ExpiresAt: time.Now().UTC().Add(timeout)}
	encoded, _ := json.Marshal(invocation)
	branches := make([]commandBranch, 0, len(assignment.Targets))
	for _, target := range assignment.Targets {
		branches = append(branches, commandBranch{target: target, commandID: assignment.Details.CommandID})
	}
	driver := commandDriver{handler: h, invocation: invocation, timeout: timeout}
	engine, _ := fanout.NewFanoutEngine(fanout.Config{MaxConcurrentBranches: 16, QueueDepth: defaultQueueDepth,
		ContinueOnBranchError: true}, driver)
	run, err := engine.Run(ctx, assignment.Source, branches, nil, fanout.Lifecycle[commandBranch, contracts.CommandOutcome]{})
	if err != nil {
		return domain.Result{}, err
	}
	details := contracts.CommandProgressDetails{Outcomes: make([]contracts.CommandOutcome, 0, len(branches))}
	for _, branch := range branches {
		if outcome, ok := run.Branches[branch.target.MemberID]; ok {
			details.Outcomes = append(details.Outcomes, outcome)
		}
	}
	report(ctx, details)
	return result(details, int64(len(encoded))), nil
}
