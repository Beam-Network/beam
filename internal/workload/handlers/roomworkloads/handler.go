package roomworkloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

const (
	defaultQueueDepth = 64
	maximumJSONBytes  = 64 << 20
)

type Handler struct {
	kind   domain.Kind
	client *http.Client
}

func NewDatagramHandler(client *http.Client) *Handler {
	return newHandler(domain.KindRoomDatagram, client)
}
func NewMessageHandler(client *http.Client) *Handler {
	return newHandler(domain.KindRoomMessage, client)
}
func NewCommandHandler(client *http.Client) *Handler {
	return newHandler(domain.KindRoomCommand, client)
}
func NewStreamHandler(client *http.Client) *Handler { return newHandler(domain.KindRoomStream, client) }

func newHandler(kind domain.Kind, client *http.Client) *Handler {
	if client == nil {
		client = &http.Client{}
	}
	return &Handler{kind: kind, client: client}
}

func (h *Handler) Kind() domain.Kind { return h.kind }

func (h *Handler) Validate(spec domain.Spec) error {
	if spec.Kind != h.kind {
		return errors.New("room workload handler received another kind")
	}
	switch h.kind {
	case domain.KindRoomDatagram:
		value, err := decodeSpec[contracts.DatagramUnitDetails](spec)
		if err != nil {
			return err
		}
		if value.Details.TTLMS <= 0 || value.Details.TTLMS > 3_600_000 || value.Details.MaxPacketBytes <= 0 || value.Details.MaxPacketBytes > 65_507 {
			return errors.New("invalid room.datagram details")
		}
	case domain.KindRoomMessage:
		value, err := decodeSpec[contracts.MessageUnitDetails](spec)
		if err != nil {
			return err
		}
		if value.Details.MessageID == "" || value.Details.ContentType == "" || value.Details.SizeBytes <= 0 {
			return errors.New("invalid room.message details")
		}
	case domain.KindRoomCommand:
		value, err := decodeSpec[contracts.CommandUnitDetails](spec)
		if err != nil {
			return err
		}
		if value.Details.CommandID == "" || value.Details.Command == "" || value.Details.Request == nil ||
			value.Details.TimeoutMS <= 0 || value.Details.TimeoutMS > 3_600_000 {
			return errors.New("invalid room.command details")
		}
	case domain.KindRoomStream:
		value, err := decodeSpec[contracts.StreamUnitDetails](spec)
		if err != nil {
			return err
		}
		if value.Details.SessionID == "" || value.Details.Replay || value.Details.Protocol == "" || value.Details.MaxBufferBytes <= 0 ||
			(value.Details.BackpressurePolicy != "drop_oldest" && value.Details.BackpressurePolicy != "drop_newest" && value.Details.BackpressurePolicy != "block") {
			return errors.New("invalid room.stream details")
		}
	default:
		return errors.New("unsupported logical room workload kind")
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	if err := h.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	switch h.kind {
	case domain.KindRoomDatagram:
		return h.executeDatagram(ctx, spec)
	case domain.KindRoomMessage:
		return h.executeMessage(ctx, spec)
	case domain.KindRoomCommand:
		return h.executeCommand(ctx, spec)
	case domain.KindRoomStream:
		return h.executeStream(ctx, spec)
	default:
		return domain.Result{}, errors.New("unsupported logical room workload kind")
	}
}

func decodeSpec[T any](spec domain.Spec) (contracts.RoomWorkerSpec[T], error) {
	var value contracts.RoomWorkerSpec[T]
	if err := json.Unmarshal(spec.Payload, &value); err != nil {
		return value, err
	}
	if value.Schema != contracts.RoomWorkloadSchema || value.Identity.WorkloadID == "" || value.Identity.UnitID == "" ||
		value.Source.PathID == "" || len(value.Targets) == 0 || len(value.Targets) > 10_000 {
		return value, errors.New("room worker specification is incomplete")
	}
	return value, nil
}

func (h *Handler) openSource(ctx context.Context, lease contracts.RoomPathLease) (*http.Response, error) {
	var failures []error
	for _, endpoint := range lease.Endpoints {
		method := endpoint.Method
		if method == "" {
			method = http.MethodGet
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint.URL, nil)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		copyHeaders(request.Header, endpoint.Headers)
		response, err := h.client.Do(request)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			response.Body.Close()
			failures = append(failures, fmt.Errorf("source returned HTTP %d", response.StatusCode))
			continue
		}
		return response, nil
	}
	return nil, fmt.Errorf("all room source endpoints failed: %w", errors.Join(failures...))
}

func (h *Handler) readSource(ctx context.Context, lease contracts.RoomPathLease, maximum int64) ([]byte, error) {
	response, err := h.openSource(ctx, lease)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	value, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > maximum {
		return nil, errors.New("room source payload exceeds its bound")
	}
	return value, nil
}

func (h *Handler) send(ctx context.Context, lease contracts.RoomPathLease, payload []byte, maximumResponse int64, decoded any) error {
	return h.sendWithHeaders(ctx, lease, payload, maximumResponse, decoded, nil)
}

func (h *Handler) sendWithHeaders(ctx context.Context, lease contracts.RoomPathLease, payload []byte,
	maximumResponse int64, decoded any, headers map[string]string) error {
	var failures []error
	for _, endpoint := range lease.Endpoints {
		method := endpoint.Method
		if method == "" {
			method = http.MethodPost
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint.URL, bytes.NewReader(payload))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		copyHeaders(request.Header, endpoint.Headers)
		copyHeaders(request.Header, headers)
		request.Header.Set("Content-Type", "application/json")
		response, err := h.client.Do(request)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumResponse+1))
		response.Body.Close()
		if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 || int64(len(body)) > maximumResponse {
			failures = append(failures, fmt.Errorf("target delivery failed with HTTP %d", response.StatusCode))
			continue
		}
		if decoded != nil && len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, decoded); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		return nil
	}
	return fmt.Errorf("all room target endpoints failed: %w", errors.Join(failures...))
}

func copyHeaders(destination http.Header, source map[string]string) {
	for key, value := range source {
		destination.Set(key, value)
	}
}

func report[T any](ctx context.Context, value T) {
	encoded, _ := json.Marshal(value)
	workloadprogress.Report(ctx, map[string]string{"room_progress_details": string(encoded)})
}
func result[T any](value T, bytesProcessed int64) domain.Result {
	encoded, _ := json.Marshal(value)
	return domain.Result{BytesProcessed: bytesProcessed, Outputs: map[string]string{"room_result_details": string(encoded)}}
}
