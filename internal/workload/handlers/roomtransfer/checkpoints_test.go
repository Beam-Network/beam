package roomtransfer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestLaneCheckpointsCoalesceChangesWithoutBlockingTheLane(t *testing.T) {
	var mu sync.Mutex
	var saved []checkpointValue
	entered, release := make(chan struct{}, 8), make(chan struct{})
	ctx := workloadcheckpoint.WithManager(context.Background(),
		&domain.Checkpoint{WorkloadID: "lane", AttemptID: "attempt", Kind: domain.KindRoomTransfer},
		func(checkpoint domain.Checkpoint) error {
			entered <- struct{}{}
			<-release
			var value checkpointValue
			if err := json.Unmarshal(checkpoint.Payload, &value); err != nil {
				return err
			}
			mu.Lock()
			saved = append(saved, value)
			mu.Unlock()
			return nil
		})
	resume := checkpointValue{}
	initializeCheckpoint(&resume)
	writer := newLaneCheckpoints(ctx, contracts.RoomTransferSchemaVersion, resume)
	deliver := func(index int64) {
		key := receiptKey("member", index)
		receipt := contracts.TargetRangeReceipt{TargetMemberID: "member", SourceRangeReceipt: contracts.SourceRangeReceipt{ChunkIndex: index}}
		writer.record(func(value *checkpointValue) { value.TargetReceipts[key] = receipt }, key, nil)
	}
	deliver(0)
	<-entered
	started := time.Now()
	for index := int64(1); index <= 4; index++ {
		deliver(index)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("recording a delivery waited for the checkpoint save")
	}
	release <- struct{}{}
	<-entered
	release <- struct{}{}
	writer.close()
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 2 || len(saved[0].TargetReceipts) != 1 || len(saved[1].TargetReceipts) != 5 {
		t.Fatalf("saves were not coalesced: %d saves", len(saved))
	}
}

func TestLaneCheckpointsReportTheFirstSaveFailure(t *testing.T) {
	failure := errors.New("disk full")
	ctx := workloadcheckpoint.WithManager(context.Background(),
		&domain.Checkpoint{WorkloadID: "lane", AttemptID: "attempt", Kind: domain.KindRoomTransfer},
		func(domain.Checkpoint) error { return failure })
	resume := checkpointValue{}
	initializeCheckpoint(&resume)
	writer := newLaneCheckpoints(ctx, contracts.RoomTransferSchemaVersion, resume)
	writer.record(func(value *checkpointValue) {}, "member:0", nil)
	deadline := time.Now().Add(5 * time.Second)
	for writer.failure() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	writer.close()
	if !errors.Is(writer.failure(), failure) {
		t.Fatalf("failure=%v", writer.failure())
	}
}
