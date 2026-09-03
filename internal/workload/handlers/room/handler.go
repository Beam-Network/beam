package room

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

const defaultMaxDatagramBytes = 1200
const roomCheckpointSchema = "beam.tunnel.room/1"

type roomCheckpoint struct {
	RoomID        string            `json:"room_id"`
	ListenAddress string            `json:"listen_address,omitempty"`
	LastSequence  map[string]uint64 `json:"last_sequence,omitempty"`
	Packets       int64             `json:"packets"`
	Dropped       int64             `json:"dropped"`
	Bytes         int64             `json:"bytes"`
}

type Config struct {
	AllowPublicListeners bool
	PacketsPerSecond     int
}

type Activation struct{ Address string }

type Handler struct {
	config Config
	mu     sync.RWMutex
	active map[string]Activation
}

type inboundDatagram struct {
	MemberID string `json:"member_id"`
	Sequence uint64 `json:"sequence"`
	Payload  []byte `json:"payload"`
	MAC      string `json:"mac"`
}

type outboundDatagram struct {
	RoomID   string `json:"room_id"`
	MemberID string `json:"member_id"`
	Sequence uint64 `json:"sequence"`
	Payload  []byte `json:"payload"`
	MAC      string `json:"mac"`
}

func NewHandler(config Config) *Handler {
	if config.PacketsPerSecond <= 0 {
		config.PacketsPerSecond = 500
	}
	return &Handler{config: config, active: make(map[string]Activation)}
}

func (h *Handler) Kind() domain.Kind { return domain.KindRoomMedia }

func (h *Handler) Activation(key string) (Activation, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	activation, ok := h.active[key]
	return activation, ok
}

func (h *Handler) Validate(spec domain.Spec) error {
	assignment, err := decodeAssignment(spec)
	if err != nil {
		return err
	}
	if assignment.Protocol != "udp" {
		return errors.New("room media handler currently requires UDP datagrams")
	}
	if assignment.Role != "relay" && assignment.Role != "shard" {
		return errors.New("room media role must be relay or shard")
	}
	if len(assignment.Members) == 0 {
		return errors.New("room media assignment requires authorized members")
	}
	for memberID, token := range assignment.Members {
		if memberID == "" || token == "" {
			return errors.New("room member ids and tokens cannot be empty")
		}
	}
	if err := validateListener(assignment.ListenAddress, h.config.AllowPublicListeners); err != nil {
		return err
	}
	if assignment.MaxDatagramBytes < 0 || assignment.MaxDatagramBytes > 64<<10 {
		return errors.New("invalid room datagram limit")
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	assignment, err := decodeAssignment(spec)
	if err != nil {
		return domain.Result{}, err
	}
	if err := h.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	resume := roomCheckpoint{RoomID: assignment.RoomID, LastSequence: make(map[string]uint64)}
	if _, ok, checkpointErr := workloadcheckpoint.Current(ctx, roomCheckpointSchema, &resume); checkpointErr != nil {
		return domain.Result{}, checkpointErr
	} else if ok && resume.RoomID != assignment.RoomID {
		return domain.Result{}, errors.New("room checkpoint belongs to another room")
	}
	if resume.LastSequence == nil {
		resume.LastSequence = make(map[string]uint64)
	}
	maximum := assignment.MaxDatagramBytes
	if maximum == 0 {
		maximum = defaultMaxDatagramBytes
	}
	address, err := net.ResolveUDPAddr("udp", assignment.ListenAddress)
	if err != nil {
		return domain.Result{}, err
	}
	connection, err := net.ListenUDP("udp", address)
	if err != nil {
		return domain.Result{}, err
	}
	defer connection.Close()
	h.mu.Lock()
	h.active[spec.Key()] = Activation{Address: connection.LocalAddr().String()}
	h.mu.Unlock()
	workloadprogress.Report(ctx, map[string]string{"listen_address": connection.LocalAddr().String(), "protocol": "udp"})
	defer func() { h.mu.Lock(); delete(h.active, spec.Key()); h.mu.Unlock() }()

	peers := make(map[string]*net.UDPAddr)
	lastSequence := resume.LastSequence
	rate := make(map[string]*rateWindow)
	var packets atomic.Int64
	var dropped atomic.Int64
	var transferred atomic.Int64
	packets.Store(resume.Packets)
	dropped.Store(resume.Dropped)
	transferred.Store(resume.Bytes)
	if err := saveRoomCheckpoint(ctx, assignment.RoomID, connection.LocalAddr().String(), lastSequence, &packets, &dropped, &transferred); err != nil {
		return domain.Result{}, err
	}
	buffer := make([]byte, maximum*2+1024)
	lastActivity := time.Now()
	lastProgress := time.Now()
	idleTimeout := time.Duration(assignment.IdleTimeoutSeconds) * time.Second
	for {
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		read, remote, readErr := connection.ReadFromUDP(buffer)
		if readErr != nil {
			if ctx.Err() != nil {
				break
			}
			if networkError, ok := readErr.(net.Error); ok && networkError.Timeout() {
				if idleTimeout > 0 && time.Since(lastActivity) >= idleTimeout {
					break
				}
				continue
			}
			return domain.Result{}, readErr
		}
		var datagram inboundDatagram
		if json.Unmarshal(buffer[:read], &datagram) != nil || len(datagram.Payload) > maximum ||
			!memberAuthorized(assignment.RoomID, assignment.Members, datagram) {
			dropped.Add(1)
			continue
		}
		if datagram.Sequence <= lastSequence[datagram.MemberID] {
			dropped.Add(1)
			continue
		}
		window := rate[datagram.MemberID]
		if window == nil {
			window = &rateWindow{}
			rate[datagram.MemberID] = window
		}
		if !window.Allow(time.Now(), h.config.PacketsPerSecond) {
			dropped.Add(1)
			continue
		}
		lastSequence[datagram.MemberID] = datagram.Sequence
		peers[datagram.MemberID] = remote
		lastActivity = time.Now()
		packets.Add(1)
		transferred.Add(int64(len(datagram.Payload)))
		if assignment.Metadata["room_workload_id"] != "" && time.Since(lastProgress) >= time.Second {
			details, _ := json.Marshal(contracts.MediaProgressDetails{Sequence: uint64(packets.Load()), Packets: packets.Load(),
				Bytes: transferred.Load(), Dropped: dropped.Load(), Tracks: []contracts.MediaTrackCounters{}})
			workloadprogress.Report(ctx, map[string]string{"room_progress_details": string(details)})
			lastProgress = time.Now()
		}
		// Persist the anti-replay cursor before forwarding the datagram. A
		// restart may lose learned peer addresses, but never sequence authority.
		if err := saveRoomCheckpoint(ctx, assignment.RoomID, connection.LocalAddr().String(), lastSequence, &packets, &dropped, &transferred); err != nil {
			return domain.Result{}, err
		}
		for memberID, peer := range peers {
			if memberID != datagram.MemberID {
				outboundMessage := outboundDatagram{
					RoomID: assignment.RoomID, MemberID: datagram.MemberID,
					Sequence: datagram.Sequence, Payload: datagram.Payload,
				}
				outboundMessage.MAC = datagramMAC(assignment.Members[memberID], assignment.RoomID,
					datagram.MemberID, datagram.Sequence, datagram.Payload)
				outbound, _ := json.Marshal(outboundMessage)
				_, _ = connection.WriteToUDP(outbound, peer)
			}
		}
	}
	outputs := map[string]string{
		"listen_address": connection.LocalAddr().String(),
		"packets":        strconv.FormatInt(packets.Load(), 10), "dropped": strconv.FormatInt(dropped.Load(), 10),
	}
	if assignment.Metadata["room_workload_id"] != "" {
		details, _ := json.Marshal(contracts.MediaResultDetails{Sequence: uint64(packets.Load()), Packets: packets.Load(),
			Bytes: transferred.Load(), Dropped: dropped.Load(), Tracks: []contracts.MediaTrackCounters{}, TerminalReason: "source_closed"})
		outputs["room_result_details"] = string(details)
	}
	return domain.Result{BytesProcessed: transferred.Load(), Outputs: outputs}, nil
}

func saveRoomCheckpoint(ctx context.Context, roomID, address string, sequences map[string]uint64, packets, dropped, transferred *atomic.Int64) error {
	sequenceCopy := make(map[string]uint64, len(sequences))
	for memberID, sequence := range sequences {
		sequenceCopy[memberID] = sequence
	}
	value := roomCheckpoint{
		RoomID: roomID, ListenAddress: address, LastSequence: sequenceCopy,
		Packets: packets.Load(), Dropped: dropped.Load(), Bytes: transferred.Load(),
	}
	err := workloadcheckpoint.Save(ctx, roomCheckpointSchema, map[string]string{"room_id": roomID, "listen_address": address}, value)
	if errors.Is(err, workloadcheckpoint.ErrUnavailable) {
		return nil
	}
	return err
}

func decodeAssignment(spec domain.Spec) (contracts.RoomAssignment, error) {
	var assignment contracts.RoomAssignment
	if err := json.Unmarshal(spec.Payload, &assignment); err != nil {
		return assignment, err
	}
	if assignment.RoomID == "" || assignment.ListenAddress == "" {
		return assignment, errors.New("room id and listen address are required")
	}
	return assignment, nil
}

func validateListener(address string, allowPublic bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("room listen address must be host:port")
	}
	ip := net.ParseIP(host)
	public := !(host == "localhost" || (ip != nil && ip.IsLoopback()))
	if public && !allowPublic {
		return errors.New("public room listeners are disabled")
	}
	return nil
}

func memberAuthorized(roomID string, members map[string]string, datagram inboundDatagram) bool {
	secret := members[datagram.MemberID]
	if secret == "" || datagram.MAC == "" {
		return false
	}
	expected := datagramMAC(secret, roomID, datagram.MemberID, datagram.Sequence, datagram.Payload)
	return hmac.Equal([]byte(expected), []byte(datagram.MAC))
}

func datagramMAC(secret, roomID, memberID string, sequence uint64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(roomID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(memberID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strconv.FormatUint(sequence, 10)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type rateWindow struct {
	started time.Time
	count   int
}

func (w *rateWindow) Allow(now time.Time, maximum int) bool {
	if w.started.IsZero() || now.Sub(w.started) >= time.Second {
		w.started = now
		w.count = 0
	}
	if w.count >= maximum {
		return false
	}
	w.count++
	return true
}
