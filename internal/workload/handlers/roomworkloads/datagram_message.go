package roomworkloads

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/fanout"
)

type datagramBranch struct {
	target contracts.RoomWorkerTarget
	frame  datagramFrame
}

type datagramFrame struct {
	Sequence  uint64    `json:"sequence"`
	ExpiresAt time.Time `json:"expires_at"`
	Payload   []byte    `json:"payload"`
}

type datagramDriver struct{ handler *Handler }

func (d datagramDriver) BranchID(value datagramBranch) string { return value.target.MemberID }
func (d datagramDriver) GroupKey(value datagramBranch) string {
	return strconv.FormatUint(value.frame.Sequence, 10)
}
func (d datagramDriver) Read(_ context.Context, _ contracts.RoomPathLease, value datagramBranch) ([]byte, error) {
	return json.Marshal(value.frame)
}
func (d datagramDriver) Deliver(ctx context.Context, value datagramBranch, payload []byte) (struct{}, error) {
	return struct{}{}, d.handler.send(ctx, value.target.Path, payload, 4<<10, nil)
}

func (h *Handler) executeDatagram(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	assignment, _ := decodeSpec[contracts.DatagramUnitDetails](spec)
	session, cancel := context.WithTimeout(ctx, time.Duration(assignment.Details.TTLMS)*time.Millisecond)
	defer cancel()
	response, err := h.openSource(session, assignment.Source)
	if err != nil {
		return domain.Result{}, err
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 128<<10)
	var counters contracts.DatagramResultDetails
	var reported contracts.DatagramResultDetails
	lastProgress := time.Now()
	for scanner.Scan() {
		var frame datagramFrame
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.Sequence == 0 || frame.ExpiresAt.IsZero() ||
			len(frame.Payload) == 0 || len(frame.Payload) > assignment.Details.MaxPacketBytes {
			counters.Dropped++
			continue
		}
		counters.Packets++
		counters.Bytes += int64(len(frame.Payload))
		branches := make([]datagramBranch, 0, len(assignment.Targets))
		for _, target := range assignment.Targets {
			branches = append(branches, datagramBranch{target: target, frame: frame})
		}
		engine, _ := fanout.NewFanoutEngine(fanout.Config{MaxConcurrentBranches: 16, QueueDepth: defaultQueueDepth,
			ContinueOnBranchError: true}, datagramDriver{handler: h})
		run, runErr := engine.Run(session, assignment.Source, branches, nil, fanout.Lifecycle[datagramBranch, struct{}]{})
		if runErr != nil && session.Err() != nil {
			break
		}
		counters.Dropped += int64(len(run.Failures))
		if time.Since(lastProgress) >= time.Second {
			details := contracts.DatagramProgressDetails{PacketsDelta: counters.Packets - reported.Packets,
				BytesDelta: counters.Bytes - reported.Bytes, DroppedDelta: counters.Dropped - reported.Dropped}
			report(ctx, details)
			reported = counters
			lastProgress = time.Now()
		}
	}
	if err := scanner.Err(); err != nil && session.Err() == nil {
		return result(counters, counters.Bytes), err
	}
	return result(counters, counters.Bytes), nil
}

type messageBranch struct {
	target contracts.RoomWorkerTarget
}
type messageRecord struct {
	MessageID string `json:"message_id"`
	Payload   []byte `json:"payload"`
}
type messageDriver struct {
	handler *Handler
	payload []byte
}

func (d messageDriver) BranchID(value messageBranch) string { return value.target.MemberID }
func (d messageDriver) GroupKey(messageBranch) string       { return "message" }
func (d messageDriver) Read(context.Context, contracts.RoomPathLease, messageBranch) ([]byte, error) {
	return d.payload, nil
}
func (d messageDriver) Deliver(ctx context.Context, value messageBranch, payload []byte) (contracts.MessageDelivery, error) {
	err := d.handler.send(ctx, value.target.Path, payload, 4<<10, nil)
	result := contracts.MessageDelivery{TargetMemberID: value.target.MemberID, State: "delivered"}
	if err != nil {
		reason := err.Error()
		if len(reason) > 512 {
			reason = reason[:512]
		}
		result.State, result.Reason = "failed", &reason
	}
	return result, nil
}

func (h *Handler) executeMessage(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	assignment, _ := decodeSpec[contracts.MessageUnitDetails](spec)
	payload, err := h.readSource(ctx, assignment.Source, maximumJSONBytes)
	if err != nil {
		return domain.Result{}, err
	}
	if !json.Valid(payload) {
		return domain.Result{}, errors.New("room.message source returned invalid JSON")
	}
	var record messageRecord
	if json.Unmarshal(payload, &record) != nil || record.MessageID == "" {
		// Preserve the original worker source contract while normalizing every
		// target delivery to the typed agent adapter contract.
		if int64(len(payload)) != assignment.Details.SizeBytes {
			return domain.Result{}, errors.New("room.message source returned an invalid record")
		}
		record = messageRecord{MessageID: assignment.Details.MessageID, Payload: payload}
		payload, _ = json.Marshal(record)
	} else if record.MessageID != assignment.Details.MessageID || int64(len(record.Payload)) != assignment.Details.SizeBytes {
		return domain.Result{}, errors.New("room.message source returned an invalid record")
	}
	branches := make([]messageBranch, 0, len(assignment.Targets))
	for _, target := range assignment.Targets {
		branches = append(branches, messageBranch{target: target})
	}
	engine, _ := fanout.NewFanoutEngine(fanout.Config{MaxConcurrentBranches: 16, QueueDepth: defaultQueueDepth,
		ContinueOnBranchError: true}, messageDriver{handler: h, payload: payload})
	run, err := engine.Run(ctx, assignment.Source, branches, nil, fanout.Lifecycle[messageBranch, contracts.MessageDelivery]{})
	if err != nil {
		return domain.Result{}, err
	}
	details := contracts.MessageProgressDetails{Deliveries: make([]contracts.MessageDelivery, 0, len(branches))}
	for _, branch := range branches {
		if delivery, ok := run.Branches[branch.target.MemberID]; ok {
			details.Deliveries = append(details.Deliveries, delivery)
		}
	}
	report(ctx, details)
	return result(details, int64(len(payload))), nil
}
