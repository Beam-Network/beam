package roomtransfer

import (
	"context"
	"maps"
	"sync"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

type laneCheckpoints struct {
	ctx    context.Context
	schema string
	value  checkpointValue

	mu        sync.Mutex
	pending   []func(*checkpointValue)
	lastRange string
	progress  map[string]string
	err       error

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

func newLaneCheckpoints(ctx context.Context, schema string, resume checkpointValue) *laneCheckpoints {
	writer := &laneCheckpoints{ctx: ctx, schema: schema, value: cloneCheckpointValue(resume),
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go writer.run()
	return writer
}

func (w *laneCheckpoints) queue(change func(*checkpointValue)) {
	w.mu.Lock()
	w.pending = append(w.pending, change)
	w.mu.Unlock()
}

func (w *laneCheckpoints) record(change func(*checkpointValue), lastRange string, progress map[string]string) {
	w.mu.Lock()
	w.pending = append(w.pending, change)
	w.lastRange, w.progress = lastRange, progress
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *laneCheckpoints) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *laneCheckpoints) close() {
	close(w.stop)
	<-w.done
}

func (w *laneCheckpoints) run() {
	defer close(w.done)
	for {
		select {
		case <-w.stop:
			return
		case <-w.wake:
		}
		w.mu.Lock()
		changes, lastRange, progress := w.pending, w.lastRange, w.progress
		w.pending, w.progress = nil, nil
		w.mu.Unlock()
		if len(changes) == 0 {
			continue
		}
		for _, change := range changes {
			change(&w.value)
		}
		err := saveCheckpoint(w.ctx, w.schema, w.value, lastRange)
		if progress != nil {
			workloadprogress.Report(w.ctx, progress)
		}
		if err != nil {
			w.mu.Lock()
			if w.err == nil {
				w.err = err
			}
			w.mu.Unlock()
		}
	}
}

func cloneCheckpointValue(value checkpointValue) checkpointValue {
	value.SourceReads = maps.Clone(value.SourceReads)
	value.SourceReceipts = maps.Clone(value.SourceReceipts)
	value.TargetReceipts = maps.Clone(value.TargetReceipts)
	value.FinalReceipts = maps.Clone(value.FinalReceipts)
	value.StorageResults = maps.Clone(value.StorageResults)
	initializeCheckpoint(&value)
	return value
}

func recordStorageResult(evidence contracts.StorageRangeResult) func(*checkpointValue) {
	return func(value *checkpointValue) { value.StorageResults[storageResultKey(evidence)] = evidence }
}
