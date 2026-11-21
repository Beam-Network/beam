package roomtransfer

import (
	"context"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

const routeReuseMargin = 5 * time.Second

type sourceAhead struct {
	index    int64
	read     bool
	cancel   context.CancelFunc
	done     chan struct{}
	route    contracts.StorageRoute
	payload  []byte
	evidence contracts.StorageRangeResult
	err      error
}

func (h *Handler) startSourceAhead(ctx context.Context, active *session, workerID string, index int64, read bool) *sourceAhead {
	ctx, cancel := context.WithCancel(ctx)
	ahead := &sourceAhead{index: index, read: read, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(ahead.done)
		transfer := active.current()
		ahead.route, ahead.err = resolveStorageRoute(ctx, h.storageClient, transfer, workerID, transfer.SourceLease, index, "", h.now())
		if ahead.err == nil && read {
			ahead.payload, ahead.evidence, ahead.err = readStorageRoute(ctx, h.storageClient, transfer, ahead.route, index, h.now)
		}
	}()
	return ahead
}

func (a *sourceAhead) stop() {
	if a == nil {
		return
	}
	a.cancel()
	<-a.done
}

func (h *Handler) sourceChunk(ctx context.Context, active *session, workerID string, index int64,
	ahead *sourceAhead) ([]byte, contracts.StorageRangeResult, error) {
	if ahead != nil && ahead.index == index {
		<-ahead.done
		ahead.cancel()
		if ahead.err == nil && ahead.read {
			return ahead.payload, ahead.evidence, nil
		}
		if ahead.err == nil && h.now().Add(routeReuseMargin).Before(ahead.route.ExpiresAt) {
			return readStorageRoute(ctx, h.storageClient, active.current(), ahead.route, index, h.now)
		}
	} else {
		ahead.stop()
	}
	return readStorageChunk(ctx, h.storageClient, active.current(), workerID, index, h.now)
}

func nextUndelivered(resume checkpointValue, transfer contracts.RoomTransfer, from int64) (int64, bool) {
	for index := from; index <= transfer.ChunkEnd; index++ {
		if !allTargetsDelivered(resume, transfer.Targets, index) {
			return index, true
		}
	}
	return 0, false
}
