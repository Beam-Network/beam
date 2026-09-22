package connectors

import (
	"context"
	"log"
	"sync"
)

const roomWorkloadParallelism = 16
const roomWorkloadPendingLimit = 64

type roomWorkloadJob struct {
	workloadID string
	payload    []byte
	cancel     bool
}

// roomWorkloadQueue admits a bounded number of control messages. One workload
// keeps its offer/cancel order, while unrelated workloads can use any free
// worker; there is no hash-lane collision between their network operations.
type roomWorkloadQueue struct {
	input   chan roomWorkloadJob
	slots   chan struct{}
	process func(context.Context, roomWorkloadJob) error
}

func newRoomWorkloadQueue(process func(context.Context, roomWorkloadJob) error) *roomWorkloadQueue {
	return &roomWorkloadQueue{
		input:   make(chan roomWorkloadJob, roomWorkloadPendingLimit),
		slots:   make(chan struct{}, roomWorkloadPendingLimit),
		process: process,
	}
}

func (q *roomWorkloadQueue) enqueue(ctx context.Context, job roomWorkloadJob) error {
	select {
	case q.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case q.input <- job:
		return nil
	case <-ctx.Done():
		<-q.slots
		return ctx.Err()
	}
}

func (q *roomWorkloadQueue) run(ctx context.Context) {
	jobs := make(chan roomWorkloadJob)
	completed := make(chan string, roomWorkloadParallelism)
	var workers sync.WaitGroup
	for range roomWorkloadParallelism {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-jobs:
					if err := q.process(ctx, job); err != nil {
						log.Printf("ignore invalid BeamCore room workload message: %v", err)
					}
					select {
					case completed <- job.workloadID:
						<-q.slots
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer workers.Wait()

	pending := make(map[string][]roomWorkloadJob)
	active := make(map[string]bool)
	ready := make([]string, 0)
	for {
		var next chan roomWorkloadJob
		var job roomWorkloadJob
		if len(ready) > 0 {
			job = pending[ready[0]][0]
			next = jobs
		}
		select {
		case <-ctx.Done():
			return
		case incoming := <-q.input:
			key := incoming.workloadID
			if len(pending[key]) == 0 && !active[key] {
				ready = append(ready, key)
			}
			pending[key] = append(pending[key], incoming)
		case key := <-completed:
			delete(active, key)
			if len(pending[key]) > 0 {
				ready = append(ready, key)
			}
		case next <- job:
			key := ready[0]
			ready = ready[1:]
			pending[key] = pending[key][1:]
			if len(pending[key]) == 0 {
				delete(pending, key)
			}
			active[key] = true
		}
	}
}
