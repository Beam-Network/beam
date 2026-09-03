package roomworkloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
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

type streamBranch struct {
	target   contracts.RoomWorkerTarget
	sequence uint64
	payload  []byte
}
type streamDriver struct {
	handler *Handler
	timeout time.Duration
}

func (d streamDriver) BranchID(value streamBranch) string { return value.target.MemberID }
func (d streamDriver) GroupKey(value streamBranch) string {
	return strconv.FormatUint(value.sequence, 10)
}
func (d streamDriver) Read(_ context.Context, _ contracts.RoomPathLease, value streamBranch) ([]byte, error) {
	return value.payload, nil
}
func (d streamDriver) Deliver(ctx context.Context, value streamBranch, payload []byte) (struct{}, error) {
	requestContext, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return struct{}{}, d.handler.send(requestContext, value.target.Path, payload, 4<<10, nil)
}

func (h *Handler) executeStream(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	assignment, _ := decodeSpec[contracts.StreamUnitDetails](spec)
	response, err := h.openSource(ctx, assignment.Source)
	if err != nil {
		return domain.Result{}, err
	}
	defer response.Body.Close()
	detached := make(map[string]struct{})
	details := contracts.StreamResultDetails{Sequence: assignment.Details.ResumeFromSequence, TerminalReason: "source_closed"}
	type sourceRead struct {
		payload []byte
		err     error
	}
	reads := make(chan sourceRead, 1)
	go func() {
		defer close(reads)
		bufferSize := min(int64(32<<10), assignment.Details.MaxBufferBytes)
		buffer := make([]byte, max(int64(1), bufferSize))
		for {
			read, readErr := response.Body.Read(buffer)
			current := sourceRead{err: readErr}
			if read > 0 {
				current.payload = bytes.Clone(buffer[:read])
			}
			select {
			case reads <- current:
			case <-ctx.Done():
				return
			}
			if readErr != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return result(details, details.Bytes), ctx.Err()
		case <-ticker.C:
			report(ctx, contracts.StreamProgressDetails{Sequence: details.Sequence, Bytes: details.Bytes,
				BufferedBytes: 0, Dropped: details.Dropped})
		case current, ok := <-reads:
			if !ok {
				return result(details, details.Bytes), nil
			}
			if len(current.payload) > 0 {
				details.Sequence++
				details.Bytes += int64(len(current.payload))
				branches := make([]streamBranch, 0, len(assignment.Targets))
				for _, target := range assignment.Targets {
					if _, drop := detached[target.MemberID]; !drop {
						branches = append(branches, streamBranch{target: target, sequence: details.Sequence, payload: current.payload})
					}
				}
				if len(branches) > 0 {
					engine, _ := fanout.NewFanoutEngine(fanout.Config{MaxConcurrentBranches: 16, QueueDepth: defaultQueueDepth,
						ContinueOnBranchError: true}, streamDriver{handler: h, timeout: 2 * time.Second})
					run, runErr := engine.Run(ctx, assignment.Source, branches, nil, fanout.Lifecycle[streamBranch, struct{}]{})
					if runErr != nil && ctx.Err() != nil {
						return result(details, details.Bytes), runErr
					}
					for member := range run.Failures {
						detached[member] = struct{}{}
						details.Dropped++
					}
				}
			}
			if errors.Is(current.err, io.EOF) {
				for _, target := range assignment.Targets {
					if _, drop := detached[target.MemberID]; drop {
						continue
					}
					if err := h.sendWithHeaders(ctx, target.Path, nil, 4<<10, nil,
						map[string]string{"X-Beam-Stream-EOF": "true"}); err != nil {
						details.Dropped++
					}
				}
				return result(details, details.Bytes), nil
			}
			if current.err != nil {
				details.TerminalReason = "worker_failed"
				return result(details, details.Bytes), current.err
			}
		}
	}
}
