package roomtransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

const requestHoldLimit = 10 * time.Second

const relayAdvanceBytes = 128 << 10

type relayState int

const (
	relayReceiving relayState = iota
	relayVerified
	relayFailed
)

type relayChunk struct {
	index    int64
	receipt  contracts.SourceRangeReceipt
	payload  []byte
	mu       sync.Mutex
	received int
	state    relayState
	advanced chan struct{}
}

func newRelayChunk(index int64, receipt contracts.SourceRangeReceipt, length int64) *relayChunk {
	return &relayChunk{index: index, receipt: receipt, payload: make([]byte, length), advanced: make(chan struct{})}
}

func (r *relayChunk) advance(received int) {
	r.mu.Lock()
	r.received = received
	close(r.advanced)
	r.advanced = make(chan struct{})
	r.mu.Unlock()
}

func (r *relayChunk) finish(verified bool) {
	r.mu.Lock()
	r.state = relayFailed
	if verified {
		r.state = relayVerified
	}
	close(r.advanced)
	r.advanced = make(chan struct{})
	r.mu.Unlock()
}

func (r *relayChunk) next(sent int) ([]byte, bool, bool, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == relayFailed {
		return nil, false, true, nil
	}
	limit := r.received
	if r.state != relayVerified {
		limit = min(limit, len(r.payload)-1)
	}
	if sent < limit {
		return r.payload[sent:limit], false, false, nil
	}
	if r.state == relayVerified && sent == len(r.payload) {
		return nil, true, false, nil
	}
	return nil, false, false, r.advanced
}

func (r *relayChunk) receive(body io.Reader, protection contracts.RoomTransferProtection, plaintextLength int64) bool {
	digest := sha256.New()
	received, announced := 0, 0
	for received < len(r.payload) {
		n, err := body.Read(r.payload[received:])
		if n > 0 {
			digest.Write(r.payload[received : received+n])
			received += n
			if received-announced >= relayAdvanceBytes || received == len(r.payload) {
				r.advance(received)
				announced = received
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && received == len(r.payload) {
				break
			}
			return false
		}
	}
	var extra [1]byte
	if n, err := io.ReadFull(body, extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return false
	}
	return validProtectedChunk(r.payload, protection, plaintextLength) && hex.EncodeToString(digest.Sum(nil)) == r.receipt.RangeSHA256
}

func (r *relayChunk) stream(response http.ResponseWriter, request *http.Request, memberID string) {
	writeChunkHeaders(response, len(r.payload), r.receipt.RangeSHA256, memberID)
	response.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(response)
	_ = controller.Flush()
	sent := 0
	for {
		data, complete, failed, wait := r.next(sent)
		if failed {
			panic(http.ErrAbortHandler)
		}
		if len(data) > 0 {
			written, err := response.Write(data)
			sent += written
			if err != nil {
				return
			}
			continue
		}
		if complete {
			return
		}
		if controller.Flush() != nil {
			return
		}
		select {
		case <-wait:
		case <-request.Context().Done():
			return
		}
	}
}

func writeChunkHeaders(response http.ResponseWriter, length int, digest, memberID string) {
	header := response.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Length", strconv.Itoa(length))
	header.Set("X-Beam-Range-SHA256", digest)
	header.Set("X-Beam-Target-Member-ID", memberID)
}
