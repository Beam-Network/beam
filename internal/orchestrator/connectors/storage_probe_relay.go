package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/nats-io/nats.go"
	"github.com/vmihailenco/msgpack/v5"
)

// StorageProbeRelayLink routes storage probe relays to Workers (the WCP server).
type StorageProbeRelayLink interface {
	StorageProbeRelayAvailable() bool
	SetStorageProbeRelaySink(func(wcp.StorageProbeRelayEvent))
	OpenStorageProbeRelay(workerID string, open contracts.StorageProbeRelayOpen, expiresAt time.Time) string
	ForwardStorageProbeRelayData(contracts.StorageProbeRelayData)
	CloseStorageProbeRelay(request contracts.StorageProbeRelayClose, notifyBeamCore bool)
}

// AttachStorageProbeRelays enables storage.probe.relay.v1 through the Workers' WCP links.
func (s *BeamCoreConnector) AttachStorageProbeRelays(link StorageProbeRelayLink) { s.relays = link }

// runStorageProbeRelays forwards BeamCore relay frames in arrival order.
func (control *roomControl) runStorageProbeRelays(ctx context.Context, messages <-chan *nats.Msg) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-messages:
			if message == nil {
				return
			}
			messageType := message.Subject[strings.LastIndex(message.Subject, ".")+1:]
			if err := control.handleStorageProbeRelayMessage(messageType, message.Data, time.Now()); err != nil {
				log.Printf("ignore invalid BeamCore %s: %v", messageType, err)
			}
		}
	}
}

// handleStorageProbeRelayMessage applies one BeamCore relay message. Relay
// messages are fire-and-forget: refusals are published as relay closes.
func (control *roomControl) handleStorageProbeRelayMessage(messageType string, encoded []byte, now time.Time) error {
	var payload map[string]any
	if err := msgpack.Unmarshal(encoded, &payload); err != nil {
		return err
	}
	if payload["type"] != messageType {
		return fmt.Errorf("payload has type %v", payload["type"])
	}
	relayID, _ := payload["relay_id"].(string)
	if !contracts.ValidStorageProbeRelayID(relayID) {
		return errors.New("relay_id must be a lowercase UUID")
	}
	switch messageType {
	case contracts.StorageProbeRelayOpenType:
		workerID, open, expiresAt, reason := parseStorageProbeRelayOpen(payload, relayID, now)
		if reason == "" {
			reason = control.relays.OpenStorageProbeRelay(workerID, open, expiresAt)
		}
		if reason != "" {
			control.publishStorageProbeRelayEvent(wcp.StorageProbeRelayEvent{
				Type: contracts.StorageProbeRelayCloseType, RelayID: relayID, Reason: reason,
			})
		}
	case contracts.StorageProbeRelayDataType:
		seq, seqOK := msgpackInteger(payload["seq"])
		data, dataOK := payload["data"].(string)
		if !hasExactFields(payload, "type", "relay_id", "seq", "data") || !seqOK || seq < 0 || !dataOK {
			control.relays.CloseStorageProbeRelay(contracts.StorageProbeRelayClose{
				RelayID: relayID, Reason: contracts.StorageProbeRelayProtocolError,
			}, true)
			return errors.New("malformed relay data frame")
		}
		control.relays.ForwardStorageProbeRelayData(contracts.StorageProbeRelayData{RelayID: relayID, Seq: seq, Data: data})
	case contracts.StorageProbeRelayCloseType:
		reason, _ := payload["reason"].(string)
		if !hasExactFields(payload, "type", "relay_id", "reason") || !contracts.ValidStorageProbeRelayReason(reason) {
			reason = contracts.StorageProbeRelayProtocolError
		}
		control.relays.CloseStorageProbeRelay(contracts.StorageProbeRelayClose{RelayID: relayID, Reason: reason}, false)
	default:
		return fmt.Errorf("unknown relay message type %q", messageType)
	}
	return nil
}

// parseStorageProbeRelayOpen extracts the routing fields of an open and keeps
// the intent's values unchanged for the Worker, which verifies the signature.
func parseStorageProbeRelayOpen(payload map[string]any, relayID string, now time.Time) (
	string, contracts.StorageProbeRelayOpen, time.Time, string) {
	signature, signatureOK := payload["signature"].(string)
	intentValue, intentOK := payload["intent"].(map[string]any)
	if !hasExactFields(payload, "type", "relay_id", "intent", "signature") || !signatureOK || !intentOK {
		return "", contracts.StorageProbeRelayOpen{}, time.Time{}, contracts.StorageProbeRelayIntentInvalid
	}
	rawIntent, err := json.Marshal(intentValue)
	if err != nil {
		return "", contracts.StorageProbeRelayOpen{}, time.Time{}, contracts.StorageProbeRelayIntentInvalid
	}
	intent, err := contracts.ParseStorageProbeRelayIntent(rawIntent)
	if err != nil || intent.RelayID != relayID || intent.WorkerID == "" {
		return "", contracts.StorageProbeRelayOpen{}, time.Time{}, contracts.StorageProbeRelayIntentInvalid
	}
	_, expiresAt, err := intent.Window()
	if err != nil {
		return "", contracts.StorageProbeRelayOpen{}, time.Time{}, contracts.StorageProbeRelayIntentInvalid
	}
	if !now.Before(expiresAt) {
		return "", contracts.StorageProbeRelayOpen{}, time.Time{}, contracts.StorageProbeRelayIntentExpired
	}
	return intent.WorkerID, contracts.StorageProbeRelayOpen{RelayID: relayID, Intent: rawIntent, Signature: signature}, expiresAt, ""
}

// publishStorageProbeRelayEvent publishes one relay message to BeamCore on
// this Orchestrator's subject, without a reply subject.
func (control *roomControl) publishStorageProbeRelayEvent(event wcp.StorageProbeRelayEvent) {
	payload := map[string]any{"type": event.Type, "relay_id": event.RelayID}
	switch event.Type {
	case contracts.StorageProbeRelayDataType:
		payload["seq"], payload["data"] = event.Seq, event.Data
	case contracts.StorageProbeRelayCloseType:
		payload["reason"] = event.Reason
	}
	encoded, err := msgpack.Marshal(payload)
	if err == nil {
		err = control.conn.publish(control.subject("orch", event.Type), encoded)
	}
	if err != nil {
		log.Printf("publish BeamCore %s relay_id=%s: %v", event.Type, event.RelayID, err)
	}
}

func hasExactFields(payload map[string]any, fields ...string) bool {
	if len(payload) != len(fields) {
		return false
	}
	for _, field := range fields {
		if _, ok := payload[field]; !ok {
			return false
		}
	}
	return true
}

// msgpackInteger accepts MessagePack integers and integral floats.
func msgpackInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int8:
		return int64(number), true
	case int16:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case uint8:
		return int64(number), true
	case uint16:
		return int64(number), true
	case uint32:
		return int64(number), true
	case uint64:
		if number > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	case float32:
		return msgpackInteger(float64(number))
	case float64:
		if number != math.Trunc(number) || number < math.MinInt64 || number > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	default:
		return 0, false
	}
}
