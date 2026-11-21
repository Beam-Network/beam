package roomtransfer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

type relayFixture struct {
	t         *testing.T
	transfer  contracts.RoomTransfer
	sourceKey ed25519.PrivateKey
	active    *session
	server    *httptest.Server
	dials     atomic.Int64
	client    *http.Client
	now       time.Time
}

func newRelayFixture(t *testing.T, chunkSize int64, chunks int64) *relayFixture {
	t.Helper()
	now := time.Now().UTC()
	_, sourceKey, _ := ed25519.GenerateKey(rand.Reader)
	_, targetKey, _ := ed25519.GenerateKey(rand.Reader)
	transfer := contracts.RoomTransfer{SchemaVersion: contracts.RoomTransferSchemaVersion,
		BatchID: "batch-1", RoomID: "room-1", ChannelID: "channel-1", PublicationID: "transfer-1",
		TransferID: "transfer-1", SnapshotVersion: 1, LaneID: "lane-1", Attempt: 1,
		Protection:    contracts.RoomTransferProtection{Scheme: contracts.RoomTransferProtectionScheme, KeyEpoch: 1},
		FileSizeBytes: chunkSize * chunks, ChunkSizeBytes: chunkSize, ChunkCount: chunks, ChunkStart: 0, ChunkEnd: chunks - 1,
		SourceLease: directLease("source-lease", contracts.TunnelLeaseRoleSourceRead, "", sourceKey.Public().(ed25519.PublicKey), now),
		Targets: []contracts.RoomTransferDestination{{MemberID: "member-b",
			Lease: directLease("target-lease", contracts.TunnelLeaseRoleTargetWrite, "member-b", targetKey.Public().(ed25519.PublicKey), now)}}}
	fixture := &relayFixture{t: t, transfer: transfer, sourceKey: sourceKey, now: now}
	fixture.active = &session{transfer: transfer, token: "runtime-token", now: func() time.Time { return now },
		chunks: make(map[int64]*chunk), completed: make(map[int64]*completedChunk), changed: make(chan struct{}, 1),
		targetFailures: make(map[string]contracts.TargetFailureReceipt), closed: make(chan struct{})}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.active.ServeHTTP(w, r, strings.Split(strings.Trim(r.URL.Path, "/"), "/"))
	}))
	t.Cleanup(fixture.server.Close)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		fixture.dials.Add(1)
		return dialer.DialContext(ctx, network, address)
	}
	t.Cleanup(transport.CloseIdleConnections)
	fixture.client = &http.Client{Transport: transport}
	return fixture
}

func (f *relayFixture) protected(index int64, corrupt bool) ([]byte, string) {
	plaintext := bytes.Repeat([]byte{byte('a' + index)}, int(f.transfer.ChunkSizeBytes))
	wire := make([]byte, len(plaintext)+protectedChunkOverhead)
	wire[0] = 1
	binary.BigEndian.PutUint64(wire[1:9], f.transfer.Protection.KeyEpoch)
	copy(wire[9:], plaintext)
	receipt := contracts.SourceRangeReceipt{ReceiptID: fmt.Sprintf("receipt-%d", index), TransferID: f.transfer.TransferID,
		LaneID: f.transfer.LaneID, ChunkIndex: index, Offset: index * f.transfer.ChunkSizeBytes, Length: int64(len(plaintext)),
		RangeSHA256: digest(wire), LeaseID: f.transfer.SourceLease.LeaseID, CompletedAt: f.now}
	signSourceReceipt(&receipt, f.sourceKey)
	encoded, _ := json.Marshal(receipt)
	if corrupt {
		wire = append([]byte(nil), wire...)
		wire[len(wire)-1] ^= 0xff
	}
	return wire, base64.RawURLEncoding.EncodeToString(encoded)
}

func (f *relayFixture) upload(index int64, body io.Reader, length int, receipt string) (*http.Response, error) {
	request, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/source/chunks/%d", f.server.URL, index), body)
	request.ContentLength = int64(length)
	request.Header.Set("Authorization", "Bearer runtime-token")
	request.Header.Set("X-Beam-Path-Token", "path-token-source-lease")
	request.Header.Set("X-Beam-Source-Receipt", receipt)
	request.Header.Set("Expect", "100-continue")
	return f.client.Do(request)
}

func (f *relayFixture) download(index int64) (*http.Response, error) {
	request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/targets/member-b/chunks/%d", f.server.URL, index), nil)
	request.Header.Set("Authorization", "Bearer runtime-token")
	request.Header.Set("X-Beam-Path-Token", "path-token-target-lease")
	return f.client.Do(request)
}

func TestHeldSourceUploadIsReadWhenItsChunkIsExpectedOnOneConnection(t *testing.T) {
	fixture := newRelayFixture(t, 64<<10, 2)
	first, firstReceipt := fixture.protected(0, false)
	response, err := fixture.upload(0, bytes.NewReader(first), len(first), firstReceipt)
	if err != nil || response.StatusCode != http.StatusNoContent || response.Close {
		t.Fatalf("first upload status=%v close=%v err=%v", response, response != nil && response.Close, err)
	}
	response.Body.Close()
	released := make(chan time.Time, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		released <- time.Now()
		fixture.active.release(0)
	}()
	second, secondReceipt := fixture.protected(1, false)
	response, err = fixture.upload(1, bytes.NewReader(second), len(second), secondReceipt)
	if err != nil {
		t.Fatal(err)
	}
	answered := time.Now()
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Close {
		t.Fatalf("held upload status=%d close=%v", response.StatusCode, response.Close)
	}
	if releasedAt := <-released; answered.Before(releasedAt) {
		t.Fatal("held upload was accepted before its chunk was expected")
	}
	if stored := fixture.active.chunks[1]; stored == nil || !bytes.Equal(stored.payload, second) {
		t.Fatal("held upload was not stored")
	}
	if dials := fixture.dials.Load(); dials != 1 {
		t.Fatalf("uploads used %d connections, want 1", dials)
	}
}

func TestHeldUploadAndDownloadAnswer425AtTheHoldLimit(t *testing.T) {
	fixture := newRelayFixture(t, 64<<10, 2)
	fixture.active.hold = 150 * time.Millisecond
	second, receipt := fixture.protected(1, false)
	started := time.Now()
	response, err := fixture.upload(1, bytes.NewReader(second), len(second), receipt)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooEarly || !response.Close || time.Since(started) < 150*time.Millisecond {
		t.Fatalf("future upload status=%d close=%v after %s", response.StatusCode, response.Close, time.Since(started))
	}
	response, err = fixture.download(1)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooEarly || response.Close {
		t.Fatalf("future download status=%d close=%v", response.StatusCode, response.Close)
	}
}

func TestHeldDownloadReceivesBytesBeforeTheUploadCompletes(t *testing.T) {
	fixture := newRelayFixture(t, 1<<20, 1)
	wire, receipt := fixture.protected(0, false)
	type downloaded struct {
		response *http.Response
		err      error
	}
	responses := make(chan downloaded, 1)
	go func() {
		response, err := fixture.download(0)
		responses <- downloaded{response, err}
	}()
	reader, writer := io.Pipe()
	uploaded := make(chan error, 1)
	go func() {
		response, err := fixture.upload(0, reader, len(wire), receipt)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("upload status %d", response.StatusCode)
			}
		}
		uploaded <- err
	}()
	half := len(wire) / 2
	if _, err := writer.Write(wire[:half]); err != nil {
		t.Fatal(err)
	}
	result := <-responses
	if result.err != nil || result.response.StatusCode != http.StatusOK {
		t.Fatalf("download status=%v err=%v", result.response, result.err)
	}
	defer result.response.Body.Close()
	if result.response.ContentLength != int64(len(wire)) || result.response.Header.Get("X-Beam-Range-SHA256") != digest(wire) {
		t.Fatalf("download headers length=%d hash=%s", result.response.ContentLength, result.response.Header.Get("X-Beam-Range-SHA256"))
	}
	early := make([]byte, half-relayAdvanceBytes)
	if _, err := io.ReadFull(result.response.Body, early); err != nil || !bytes.Equal(early, wire[:len(early)]) {
		t.Fatalf("relayed bytes before the upload completed: %v", err)
	}
	if _, err := writer.Write(wire[half:]); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	rest, err := io.ReadAll(result.response.Body)
	if err != nil || !bytes.Equal(append(early, rest...), wire) {
		t.Fatalf("relayed chunk differs: %v", err)
	}
	if err := <-uploaded; err != nil {
		t.Fatal(err)
	}
}

func TestRelayWithholdsTheFinalByteUntilTheChunkIsVerified(t *testing.T) {
	relay := newRelayChunk(0, contracts.SourceRangeReceipt{}, 4)
	relay.advance(4)
	data, complete, failed, wait := relay.next(0)
	if len(data) != 3 || complete || failed {
		t.Fatalf("unverified relay released %d bytes complete=%v failed=%v", len(data), complete, failed)
	}
	if _, complete, _, wait = relay.next(3); complete || wait == nil {
		t.Fatal("unverified relay released its final byte")
	}
	relay.finish(true)
	<-wait
	if data, _, _, _ := relay.next(3); len(data) != 1 {
		t.Fatalf("verified relay withheld %d bytes", 1-len(data))
	}
	if _, complete, _, _ := relay.next(4); !complete {
		t.Fatal("verified relay did not complete")
	}
}

func TestFailedUploadAbortsTheTargetDownload(t *testing.T) {
	fixture := newRelayFixture(t, 1<<20, 1)
	wire, receipt := fixture.protected(0, true)
	type downloaded struct {
		body []byte
		err  error
	}
	downloads := make(chan downloaded, 1)
	go func() {
		response, err := fixture.download(0)
		if err != nil {
			downloads <- downloaded{nil, err}
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			downloads <- downloaded{nil, fmt.Errorf("download status %d", response.StatusCode)}
			return
		}
		body, err := io.ReadAll(response.Body)
		downloads <- downloaded{body, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fixture.active.mu.Lock()
		waiting := fixture.active.notify != nil
		fixture.active.mu.Unlock()
		if waiting || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	response, err := fixture.upload(0, bytes.NewReader(wire), len(wire), receipt)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("corrupt upload status=%d", response.StatusCode)
	}
	result := <-downloads
	if result.err == nil || !errors.Is(result.err, io.ErrUnexpectedEOF) || len(result.body) >= len(wire) {
		t.Fatalf("target received %d of %d bytes, err=%v", len(result.body), len(wire), result.err)
	}
	fixture.active.mu.Lock()
	defer fixture.active.mu.Unlock()
	if fixture.active.relay != nil || fixture.active.chunks[0] != nil {
		t.Fatal("refused upload left the chunk in the lane")
	}
}
