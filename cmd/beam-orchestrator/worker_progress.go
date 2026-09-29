package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
)

const progressWorkerCount = 16

// Progress for one attempt stays ordered, while slow upstream calls for another
// attempt cannot hold up time-limited room message runtime announcements.
func consumeWorkerProgress(ctx context.Context, events <-chan wcp.ProgressEvent,
	handle func(context.Context, wcp.ProgressEvent) error) {
	var queues [progressWorkerCount]chan wcp.ProgressEvent
	var workers sync.WaitGroup
	for index := range queues {
		queues[index] = make(chan wcp.ProgressEvent, 64)
		workers.Add(1)
		go func(queue <-chan wcp.ProgressEvent) {
			defer workers.Done()
			for event := range queue {
				started := time.Now()
				if err := handle(ctx, event); err != nil {
					log.Printf("persist Worker progress workload_id=%s: %v", event.Progress.WorkloadID, err)
				}
				if delay := started.Sub(event.Progress.ObservedAt); delay > time.Second {
					log.Printf("Worker progress delay worker_id=%s workload_id=%s queue_delay=%s processing_duration=%s",
						event.WorkerID, event.Progress.WorkloadID, delay.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
				}
				log.Printf("Worker progress worker_id=%s workload_id=%s state=%s", event.WorkerID,
					event.Progress.WorkloadID, event.Progress.State)
			}
		}(queues[index])
	}
	defer func() {
		for _, queue := range queues {
			close(queue)
		}
		workers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			queue := queues[progressShard(event.Progress.WorkloadID, event.Progress.AttemptID)]
			select {
			case queue <- event:
			case <-ctx.Done():
				return
			}
		}
	}
}

func progressShard(workloadID, attemptID string) uint32 {
	const offset uint32 = 2166136261
	const prime uint32 = 16777619
	hash := offset
	for index := 0; index < len(workloadID); index++ {
		hash = (hash ^ uint32(workloadID[index])) * prime
	}
	hash = (hash ^ 0) * prime
	for index := 0; index < len(attemptID); index++ {
		hash = (hash ^ uint32(attemptID[index])) * prime
	}
	return hash % progressWorkerCount
}
