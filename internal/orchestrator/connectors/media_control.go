package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/nats-io/nats.go"
)

const (
	mediaTopologyUpdate = mediacontracts.TopologyUpdateType
	mediaCertResult     = mediacontracts.CertResultType
)

var mediaControlFromRuntime = []string{mediaTopologyUpdate, mediaCertResult}

var mediaUpstreamTypes = map[string]bool{mediacontracts.CapacityReportType: true, mediacontracts.UsageReportType: true,
	mediacontracts.SessionEventType: true, mediacontracts.CertRequestType: true, mediacontracts.NodeOfferRequestType: true}

func (control *roomControl) RequestMediaNodeOffer(ctx context.Context, request mediacontracts.MediaNodeOfferRequest) (json.RawMessage, error) {
	encoded, err := mediacontracts.MarshalWire(request)
	if err != nil {
		return nil, err
	}
	return control.requestReply(ctx, mediacontracts.NodeOfferRequestType, json.RawMessage(encoded))
}

func mediaAcknowledgement(messageType string, encoded []byte) error {
	var acknowledgement struct {
		Accepted  *bool  `json:"accepted"`
		Duplicate *bool  `json:"duplicate"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(encoded, &acknowledgement); err != nil {
		return err
	}
	switch {
	case acknowledgement.Accepted != nil && *acknowledgement.Accepted:
		return nil
	case acknowledgement.Accepted == nil && acknowledgement.Duplicate != nil && *acknowledgement.Duplicate:
		return nil
	case acknowledgement.Accepted == nil:
		return &roomworkloads.MediaRefusal{Type: messageType, Reason: "ack_without_accepted", Transient: true}
	default:
		reason := acknowledgement.Reason
		if reason == "" {
			reason = "not_accepted"
		}
		return &roomworkloads.MediaRefusal{Type: messageType, Reason: reason}
	}
}

func capacityAck(encoded json.RawMessage) (mediacontracts.MediaAck, bool) {
	var ack mediacontracts.MediaAck
	if err := json.Unmarshal(encoded, &ack); err != nil || !ack.Accepted {
		return ack, false
	}
	ack.Type = mediacontracts.CapacityReportAckType
	if ack.EdgeDomain != nil {
		domain := strings.ToLower(strings.TrimSpace(*ack.EdgeDomain))
		if !mediacontracts.ValidHostname(domain) {
			log.Printf("BeamCore media_capacity_report_ack: ignoring invalid edge_domain %q", *ack.EdgeDomain)
			ack.EdgeDomain = nil
		} else {
			ack.EdgeDomain = &domain
		}
	}
	var required []string
	for _, hostname := range ack.CertificatesRequired {
		hostname = strings.ToLower(strings.TrimSpace(hostname))
		if mediacontracts.ValidHostname(hostname) && len(required) < mediacontracts.CertificatesRequiredMax {
			required = append(required, hostname)
		}
	}
	ack.CertificatesRequired = required
	ack.Features = carriedFeatures(ack.Features)
	return ack, true
}

var carriedAckFeatures = []string{mediacontracts.WorkerFeatureUsageV2, mediacontracts.WorkerFeatureMediaReceipts}

func carriedFeatures(announced []string) []string {
	var carried []string
	for _, feature := range announced {
		if slices.Contains(carriedAckFeatures, feature) && !slices.Contains(carried, feature) {
			carried = append(carried, feature)
		}
	}
	return carried
}

const controlSessionNotRegistered = "orchestrator_not_registered"

func mediaErrorReply(messageType, replyType, reason string) error {
	if reason == "" {
		reason = replyType
	}
	return &roomworkloads.MediaRefusal{Type: messageType, Reason: reason, Transient: reason == controlSessionNotRegistered}
}

func transientMediaError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	return transientControlError(ctx, err) || errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrDisconnected) ||
		errors.Is(err, nats.ErrConnectionReconnecting) || errors.Is(err, nats.ErrConnectionDraining) ||
		errors.Is(err, nats.ErrNoServers) || errors.Is(err, nats.ErrStaleConnection)
}

var MediaCapacityReportInterval = 30 * time.Second

const mediaRoleCheckInterval = 5 * time.Second

const (
	capacityRestartMin = time.Second
	capacityRestartMax = 30 * time.Second
)

func (control *roomControl) keepCapacityReports(ctx context.Context) {
	pause := capacityRestartMin
	for {
		started := time.Now()
		control.runMediaCapacityReports(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= capacityRestartMax {
			pause = capacityRestartMin
		}
		log.Printf("ERROR BeamCore media_capacity_report loop panicked: started again in %s", pause)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(2*pause, capacityRestartMax)
	}
}

func (control *roomControl) handleMediaControl(messageType string, encoded []byte) error {
	payload, err := decodeControlPayload(encoded, messageType)
	if err != nil {
		return err
	}
	if messageType == mediaTopologyUpdate {
		return control.workloads.HandleMediaTopologyUpdate(payload)
	}
	return control.workloads.HandleMediaCertResult(payload)
}

func (control *roomControl) submitMediaControl(ctx context.Context, messageType string, payload json.RawMessage) error {
	return control.request(ctx, messageType, payload)
}

const capacityFeaturesRetry = 10 * time.Minute

const capacityInvalidPayload = "invalid_payload"

type capacityFeatureFallback struct {
	refusedAt time.Time
}

func (f *capacityFeatureFallback) send(report mediacontracts.CapacityReport, now time.Time,
	submit func(mediacontracts.CapacityReport) (json.RawMessage, error)) (json.RawMessage, error) {
	if !f.refusedAt.IsZero() && now.Sub(f.refusedAt) < capacityFeaturesRetry {
		stripped, _ := withoutWorkerFeatures(report)
		return submit(stripped)
	}
	reply, err := submit(report)
	var refusal *roomworkloads.MediaRefusal
	if !errors.As(err, &refusal) || refusal.Transient || refusal.Reason != capacityInvalidPayload {
		return reply, err
	}
	stripped, had := withoutWorkerFeatures(report)
	if !had {
		return reply, err
	}
	f.refusedAt = now
	log.Printf("BeamCore refused media_capacity_report with Worker features: sent again without them")
	return submit(stripped)
}

func withoutWorkerFeatures(report mediacontracts.CapacityReport) (stripped mediacontracts.CapacityReport, had bool) {
	stripped = report
	stripped.Workers = make([]mediacontracts.WorkerCapacity, len(report.Workers))
	for index, worker := range report.Workers {
		had = had || len(worker.Features) > 0
		worker.Features = nil
		stripped.Workers[index] = worker
	}
	return stripped, had
}

func (control *roomControl) runMediaCapacityReports(ctx context.Context) {
	defer control.workloads.RecoverMediaPanic("capacity reports")
	ticker := time.NewTicker(mediaRoleCheckInterval)
	defer ticker.Stop()
	var lastSent time.Time
	lastFingerprint := ""
	reported := false
	var features capacityFeatureFallback
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now().UTC()
		report, fingerprint := control.workloads.MediaCapacityReport(control.config.Hotkey, now)
		if len(report.Workers) == 0 && !reported {
			continue
		}
		if fingerprint == lastFingerprint && now.Sub(lastSent) < MediaCapacityReportInterval {
			continue
		}
		reply, err := features.send(report, now, func(report mediacontracts.CapacityReport) (json.RawMessage, error) {
			encoded, err := mediacontracts.MarshalWire(report)
			if err != nil {
				return nil, err
			}
			return control.requestReply(ctx, mediacontracts.CapacityReportType, json.RawMessage(encoded))
		})
		var refusal *roomworkloads.MediaRefusal
		if errors.As(err, &refusal) && !refusal.RetryLater() {
			log.Printf("BeamCore media_capacity_report refused: %v", err)
			reported, lastSent, lastFingerprint = true, now, fingerprint
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("BeamCore media_capacity_report failed: %v", err)
			}
			continue
		}
		reported, lastSent, lastFingerprint = true, now, fingerprint
		if ack, ok := capacityAck(reply); ok {
			workers := make([]string, 0, len(report.Workers))
			for _, worker := range report.Workers {
				workers = append(workers, worker.WorkerID)
			}
			control.workloads.ForwardMediaCapacityAck(ack, workers)
		}
	}
}
