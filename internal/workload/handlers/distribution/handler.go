package distribution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

const distributionCheckpointSchema = "beam.transfer.distribute/1"

type distributionCheckpoint struct {
	TransferID string         `json:"transfer_id"`
	Phase      string         `json:"phase"`
	Result     *domain.Result `json:"result,omitempty"`
}

type Transport interface {
	Dial(context.Context, string, string, circuit.OpenRequest) (*circuit.Conn, error)
	AcceptFor(context.Context, string, string) (*circuit.Conn, error)
}

type failoverTransport interface {
	DialWithFailover(context.Context, string, string, []string, circuit.OpenRequest) (*circuit.Conn, error)
}

type Provider func() Transport

type Handler struct {
	transport Provider
	client    *http.Client
}

func NewHandler(transport Provider, client *http.Client) *Handler {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	return &Handler{transport: transport, client: client}
}

func (h *Handler) Kind() domain.Kind { return domain.KindTransferDistribute }

func (h *Handler) Validate(spec domain.Spec) error {
	var transfer contracts.DistributionTransfer
	if err := json.Unmarshal(spec.Payload, &transfer); err != nil {
		return fmt.Errorf("decode distributed transfer: %w", err)
	}
	if transfer.TransferID == "" || transfer.Length <= 0 || transfer.Offset < 0 {
		return errors.New("distributed transfer requires transfer_id, positive length, and non-negative offset")
	}
	if transfer.Root {
		if transfer.Source == nil || transfer.ParentNodeID != "" {
			return errors.New("distribution root requires a source and no parent")
		}
		if err := validateEndpoint(*transfer.Source); err != nil {
			return fmt.Errorf("source: %w", err)
		}
	} else if transfer.ParentNodeID == "" || transfer.Source != nil || transfer.CircuitID == "" {
		return errors.New("distribution relay requires parent_node_id and circuit_id")
	}
	if len(transfer.Children) > 0 && transfer.CircuitID == "" {
		return errors.New("distribution children require a circuit_id")
	}
	if len(transfer.Children) == 0 && len(transfer.Destinations) == 0 {
		return errors.New("distribution node has no output")
	}
	if len(transfer.Children) > 64 || len(transfer.Destinations) > 256 || len(transfer.Children)+len(transfer.Destinations) > 256 {
		return errors.New("distribution fan-out exceeds bounded limits")
	}
	seen := make(map[string]struct{})
	for _, child := range transfer.Children {
		if child.WorkerID == "" || child.NodeID == "" {
			return errors.New("distribution child requires worker_id and node_id")
		}
		if len(child.StandbyNodeIDs) > 8 {
			return errors.New("distribution child has too many standby nodes")
		}
		if _, exists := seen[child.NodeID]; exists {
			return errors.New("duplicate distribution child")
		}
		seen[child.NodeID] = struct{}{}
		standbys := map[string]struct{}{child.NodeID: {}}
		for _, standby := range child.StandbyNodeIDs {
			if standby == "" {
				return errors.New("distribution standby node_id is required")
			}
			if _, exists := standbys[standby]; exists {
				return errors.New("duplicate distribution standby node")
			}
			standbys[standby] = struct{}{}
		}
	}
	for _, destination := range transfer.Destinations {
		if destination.DestinationID == "" {
			return errors.New("destination_id is required")
		}
		if err := validateEndpoint(destination.Endpoint); err != nil {
			return fmt.Errorf("destination %s: %w", destination.DestinationID, err)
		}
	}
	if transfer.ExpectedSHA256 != "" {
		digest, err := hex.DecodeString(transfer.ExpectedSHA256)
		if err != nil || len(digest) != sha256.Size {
			return errors.New("expected_sha256 must be a hexadecimal SHA-256 digest")
		}
	}
	if (!transfer.Root || len(transfer.Children) > 0) && (h.transport == nil || h.transport() == nil) {
		return errors.New("Circuit transport is unavailable")
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	var transfer contracts.DistributionTransfer
	if err := json.Unmarshal(spec.Payload, &transfer); err != nil {
		return domain.Result{}, err
	}
	var resume distributionCheckpoint
	if _, ok, checkpointErr := workloadcheckpoint.Current(ctx, distributionCheckpointSchema, &resume); checkpointErr != nil {
		return domain.Result{}, checkpointErr
	} else if ok {
		if resume.TransferID != transfer.TransferID {
			return domain.Result{}, errors.New("distribution checkpoint belongs to another transfer")
		}
		if resume.Phase == "completed" && resume.Result != nil {
			return *resume.Result, nil
		}
	}
	if err := saveDistributionCheckpoint(ctx, distributionCheckpoint{TransferID: transfer.TransferID, Phase: "streaming"}); err != nil {
		return domain.Result{}, err
	}
	transport := Transport(nil)
	if h.transport != nil {
		transport = h.transport()
	}
	var outputs []io.WriteCloser
	var completions []<-chan error
	closeOutputs := func(closeError error) {
		for _, output := range outputs {
			if pipe, ok := output.(*io.PipeWriter); ok && closeError != nil {
				_ = pipe.CloseWithError(closeError)
			} else {
				_ = output.Close()
			}
		}
	}
	defer closeOutputs(context.Canceled)

	for _, child := range transfer.Children {
		request := circuit.OpenRequest{
			Protocol: contracts.DistributionProtocol,
			Metadata: map[string]string{"transfer_id": transfer.TransferID, "from_worker_id": spec.Identity.WorkerID},
		}
		var connection *circuit.Conn
		var err error
		if routed, ok := transport.(failoverTransport); ok && len(child.StandbyNodeIDs) > 0 {
			connection, err = routed.DialWithFailover(ctx, transfer.CircuitID, child.NodeID, child.StandbyNodeIDs, request)
		} else {
			connection, err = transport.Dial(ctx, transfer.CircuitID, child.NodeID, request)
		}
		if err != nil {
			return domain.Result{}, fmt.Errorf("connect child %s: %w", child.WorkerID, err)
		}
		outputs = append(outputs, connection)
	}
	for _, destination := range transfer.Destinations {
		writer, completed, err := h.destinationSink(ctx, destination, transfer.Length)
		if err != nil {
			closeOutputs(err)
			return domain.Result{}, err
		}
		outputs = append(outputs, writer)
		completions = append(completions, completed)
	}

	input, err := h.input(ctx, spec.WorkloadID, transfer, transport)
	if err != nil {
		closeOutputs(err)
		return domain.Result{}, err
	}
	defer input.Close()
	hash := sha256.New()
	writers := make([]io.Writer, 0, len(outputs)+1)
	writers = append(writers, hash)
	for _, output := range outputs {
		writers = append(writers, output)
	}
	workloadprogress.Report(ctx, map[string]string{"phase": "streaming", "outputs": fmt.Sprint(len(outputs))})
	written, copyErr := io.CopyN(io.MultiWriter(writers...), input, transfer.Length)
	closeOutputs(copyErr)
	outputs = nil
	if copyErr != nil {
		return domain.Result{BytesProcessed: written}, fmt.Errorf("stream distribution: %w", copyErr)
	}
	for _, completed := range completions {
		if err := <-completed; err != nil {
			return domain.Result{BytesProcessed: written}, err
		}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if transfer.ExpectedSHA256 != "" && !strings.EqualFold(digest, transfer.ExpectedSHA256) {
		return domain.Result{BytesProcessed: written, Outputs: map[string]string{"sha256": digest}}, errors.New("distributed transfer SHA-256 mismatch")
	}
	result := domain.Result{BytesProcessed: written, Outputs: map[string]string{
		"sha256": digest, "destinations": fmt.Sprint(len(transfer.Destinations)), "children": fmt.Sprint(len(transfer.Children)),
	}}
	if err := saveDistributionCheckpoint(ctx, distributionCheckpoint{TransferID: transfer.TransferID, Phase: "completed", Result: &result}); err != nil {
		return domain.Result{}, err
	}
	return result, nil
}

func saveDistributionCheckpoint(ctx context.Context, value distributionCheckpoint) error {
	err := workloadcheckpoint.Save(ctx, distributionCheckpointSchema, map[string]string{"transfer_id": value.TransferID, "phase": value.Phase}, value)
	if errors.Is(err, workloadcheckpoint.ErrUnavailable) {
		return nil
	}
	return err
}

func (h *Handler) input(ctx context.Context, workloadID string, transfer contracts.DistributionTransfer, transport Transport) (io.ReadCloser, error) {
	if !transfer.Root {
		connection, err := transport.AcceptFor(ctx, contracts.DistributionProtocol, workloadID)
		if err != nil {
			return nil, fmt.Errorf("accept parent Circuit: %w", err)
		}
		if connection.Peer.NodeID != transfer.ParentNodeID || connection.Metadata["transfer_id"] != transfer.TransferID {
			connection.Close()
			return nil, errors.New("incoming Circuit does not match the distribution plan")
		}
		return connection, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, transfer.Source.URL, nil)
	if err != nil {
		return nil, err
	}
	copyHeaders(request.Header, transfer.Source.Headers)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", transfer.Offset, transfer.Offset+transfer.Length-1))
	response, err := h.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch source range: %w", err)
	}
	if response.StatusCode != http.StatusPartialContent {
		response.Body.Close()
		return nil, fmt.Errorf("source ignored range: HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}

func (h *Handler) destinationSink(ctx context.Context, destination contracts.DistributionDestination, length int64) (io.WriteCloser, <-chan error, error) {
	reader, writer := io.Pipe()
	method := strings.ToUpper(strings.TrimSpace(destination.Endpoint.Method))
	if method == "" {
		method = http.MethodPut
	}
	request, err := http.NewRequestWithContext(ctx, method, destination.Endpoint.URL, reader)
	if err != nil {
		return nil, nil, err
	}
	request.ContentLength = length
	copyHeaders(request.Header, destination.Endpoint.Headers)
	completed := make(chan error, 1)
	go func() {
		response, err := h.client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				err = fmt.Errorf("destination %s returned HTTP %d", destination.DestinationID, response.StatusCode)
			}
		}
		completed <- err
	}()
	return writer, completed, nil
}

func validateEndpoint(endpoint contracts.HTTPEndpoint) error {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("endpoint must be an absolute HTTP(S) URL")
	}
	return nil
}

func copyHeaders(destination http.Header, source map[string]string) {
	for key, value := range source {
		destination.Set(key, value)
	}
}
