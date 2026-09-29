package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestConsumeWorkerProgressDoesNotBlockOtherAttempts(t *testing.T) {
	firstID, secondID := "first", "second"
	for progressShard(firstID, "attempt") == progressShard(secondID, "attempt") {
		secondID += "x"
	}
	events := make(chan wcp.ProgressEvent, 3)
	entered := make(chan struct{})
	release := make(chan struct{})
	secondHandled := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var order []string
	go func() {
		consumeWorkerProgress(context.Background(), events, func(_ context.Context, event wcp.ProgressEvent) error {
			if event.Progress.WorkloadID == firstID && event.Progress.State == "first" {
				close(entered)
				<-release
			}
			mu.Lock()
			order = append(order, string(event.Progress.State))
			mu.Unlock()
			if event.Progress.WorkloadID == secondID {
				close(secondHandled)
			}
			return nil
		})
		close(done)
	}()
	makeEvent := func(id, state string) wcp.ProgressEvent {
		return wcp.ProgressEvent{Progress: domain.Progress{WorkloadID: id, AttemptID: "attempt", State: domain.State(state), ObservedAt: time.Now()}}
	}
	events <- makeEvent(firstID, "first")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first attempt was not handled")
	}
	events <- makeEvent(firstID, "last")
	events <- makeEvent(secondID, "other")
	close(events)
	select {
	case <-secondHandled:
	case <-time.After(2 * time.Second):
		t.Fatal("another attempt was blocked behind the first")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("progress consumer did not drain")
	}
	mu.Lock()
	defer mu.Unlock()
	var firstOrder []string
	for _, state := range order {
		if state == "first" || state == "last" {
			firstOrder = append(firstOrder, state)
		}
	}
	if len(firstOrder) != 2 || firstOrder[0] != "first" || firstOrder[1] != "last" {
		t.Fatalf("same-attempt progress was reordered: %v", firstOrder)
	}
}
